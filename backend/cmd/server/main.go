package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	_ "github.com/lib/pq"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sub2api-enhance/internal/config"
	"sub2api-enhance/internal/ingress"
	"sub2api-enhance/internal/quotafollow"
	"sub2api-enhance/internal/server"
	"sub2api-enhance/internal/sub2api"
	"sub2api-enhance/internal/systemupdate"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
	"sub2api-enhance/internal/web"
	"sub2api-enhance/migrations"
	"syscall"
	"time"
)

// Version 由 GitHub 标签在发布构建时注入；本地构建不冒充正式版本。
var Version = "dev"

// Commit 标识发布源码提交。
var Commit = "unknown"

// Date 标识发布构建时间。
var Date = "unknown"

// BuildType 控制源码运行与脚本 Release 的更新边界。
var BuildType = "source"

func main() {
	digest, err := migrations.Digest()
	if err != nil {
		log.Fatal(err)
	}
	info := systemupdate.BuildInfo{Version: Version, Commit: Commit, Date: Date, BuildType: BuildType, SchemaDigest: digest}
	if len(os.Args) > 1 {
		if len(os.Args) != 2 {
			log.Fatal("仅支持 --version、--schema-digest 或 --release-info")
		}
		switch os.Args[1] {
		case "--version", "-v":
			fmt.Println(Version)
		case "--schema-digest":
			fmt.Println(digest)
		case "--release-info":
			if err := json.NewEncoder(os.Stdout).Encode(info); err != nil {
				log.Fatal(err)
			}
		default:
			log.Fatal("未知参数")
		}
		return
	}
	if err := run(info); err != nil {
		log.Printf("增强服务退出：%v", err)
		os.Exit(1)
	}
}
func run(info systemupdate.BuildInfo) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(cfg.DatabaseConnections)
	ingressDB, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer ingressDB.Close()
	ingressDB.SetMaxOpenConns(cfg.IngressConnections)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = migrations.Run(initCtx, db)
	cancel()
	if err != nil {
		return err
	}
	captures := audit.NewCaptureStore(ingressDB)
	core, err := initializeCore(db, cfg, captures)
	if err != nil {
		return err
	}
	client := sub2api.NewClient(cfg)
	core.Service.SetAccountClient(client)
	proxy, err := ingress.New(cfg.OfficialURL, captures, core.Service, sub2api.NewIdentityStore(ingressDB))
	if err != nil {
		return err
	}
	assets, err := fs.Sub(web.Assets, "dist")
	if err != nil {
		return err
	}
	// 三个调度的会话锁独占连接池，避免小后台池被锁连接占满后自锁。
	quotaLocks, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer quotaLocks.Close()
	quotaLocks.SetMaxOpenConns(3)
	quotaRepo := quotafollow.NewRepository(db)
	cache, cacheErr := quotafollow.NewRedisCache(cfg.QuotaRedisURL)
	if cacheErr != nil {
		log.Printf("额度 Redis 初始化失败：%v", cacheErr)
	}
	var cacheReader quotafollow.CacheReader
	if cache != nil {
		cacheReader = cache
		defer cache.Close()
	}
	var quotaLocation *time.Location
	if cfg.QuotaTimezone != "" {
		quotaLocation, err = time.LoadLocation(cfg.QuotaTimezone)
		if err != nil {
			log.Printf("额度原版时区无效，自然窗口暂不归因")
		}
	}
	quotaService := quotafollow.NewService(quotaRepo, sub2api.NewQuotaData(db), client, cacheReader, quotaLocation, quotaLocks, cfg.QuotaFlusherEnabled)
	quotaHandler := quotafollow.NewHandler(quotaService, quotaRepo)
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	updater, err := systemupdate.New(info, executable, os.Getenv("UPDATE_GITHUB_TOKEN"), os.Getenv("INVOCATION_ID") != "", stop)
	if err != nil {
		return err
	}
	router, err := server.Router(cfg, core.Handler, proxy, client, assets, quotaHandler, systemupdate.NewHandler(updater))
	if err != nil {
		return err
	}
	if err := core.Service.Start(ctx); err != nil {
		log.Printf("审核配置暂未应用，管理页面保留修复能力：%v", err)
	}
	quotaService.Start(ctx)
	httpServer := &http.Server{Addr: cfg.Listen, Handler: router, BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() {
		log.Printf("增强服务开始接入 listen=%s", cfg.Listen)
		done <- httpServer.ListenAndServe()
	}()
	select {
	case err := <-done:
		if err != http.ErrServerClosed {
			stop()
			_ = core.Service.Shutdown(context.Background())
			_ = quotaService.Shutdown(context.Background())
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
	quotaErr := quotaService.Shutdown(shutdown)
	auditErr := core.Service.Shutdown(shutdown)
	if quotaErr != nil {
		return quotaErr
	}
	return auditErr
}
