package server

import (
	"crypto/subtle"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"io/fs"
	"net/http"
	"strings"
	"sub2api-enhance/internal/config"
	"sub2api-enhance/internal/ingress"
	"sub2api-enhance/internal/quotafollow"
	"sub2api-enhance/internal/sub2api"
	"sub2api-enhance/internal/systemupdate"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
)

// readonlyLimiter 是只读通道的每分钟请求上限，按令牌全局计数。
const readonlyLimiterLimit = 60

// readonlyGuard 校验只读令牌并做分钟级限流。
func readonlyGuard(token string) gin.HandlerFunc {
	var mu sync.Mutex
	windowStart := time.Now()
	hits := 0
	return func(ctx *gin.Context) {
		if subtle.ConstantTimeCompare([]byte(ctx.GetHeader("X-Enhance-Readonly-Token")), []byte(token)) != 1 {
			ctx.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": "enhance_readonly_denied", "message": "只读令牌无效"})
			return
		}
		mu.Lock()
		if time.Since(windowStart) >= time.Minute {
			windowStart, hits = time.Now(), 0
		}
		hits++
		limited := hits > readonlyLimiterLimit
		mu.Unlock()
		if limited {
			ctx.Header("Retry-After", "60")
			ctx.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"code": "enhance_readonly_limited", "message": "只读接口请求过于频繁"})
			return
		}
		ctx.Next()
	}
}

func Router(c *config.Config, handler *audit.AdminHandler, proxy *ingress.Proxy, client *sub2api.Client, assets fs.FS, quota *quotafollow.Handler, updates *systemupdate.Handler) (*gin.Engine, error) {
	r := gin.New()
	r.Use(gin.Recovery())
	if err := r.SetTrustedProxies(c.TrustedProxies); err != nil {
		return nil, err
	}
	r.Use(func(ctx *gin.Context) {
		ctx.Header("Referrer-Policy", "no-referrer")
		ctx.Header("X-Content-Type-Options", "nosniff")
		ctx.Next()
	})
	auth := NewAuth(client, c.PublicOrigin)
	r.POST("/enhance/api/v1/auth/bootstrap", auth.Bootstrap)
	// 只读监控通道：独立令牌 + 简单限流，不依赖原版管理员会话。
	if c.ReadonlyToken != "" {
		readonly := r.Group("/enhance/api/v1/admin/third-party-prompt-audit/readonly", readonlyGuard(c.ReadonlyToken))
		readonly.GET("/stats", handler.GetStats)
		readonly.GET("/runtime", handler.GetRuntime)
	}
	group := r.Group("/enhance/api/v1/admin", auth.Require)
	group.GET("/groups", handler.ListGroups)
	group.GET("/session", func(c *gin.Context) { c.JSON(200, gin.H{"code": 0, "data": gin.H{"authenticated": true}}) })
	system := group.Group("/system")
	system.GET("/status", updates.Status)
	system.GET("/check-updates", updates.Check)
	system.POST("/update", updates.Update)
	system.POST("/rollback", updates.Rollback)
	system.POST("/restart", updates.Restart)
	q := group.Group("/quota-follow")
	q.GET("/events", quota.Events)
	q.GET("/events/:id", quota.Event)
	q.GET("/config", quota.GetConfig)
	q.PUT("/config", quota.SaveConfig)
	q.GET("/runtime", quota.Runtime)
	q.GET("/discovery", quota.Discovery)
	q.POST("/reset-now", quota.ImmediateReset)
	q.GET("/reset-records", quota.Records)
	q.GET("/reset-records/:id", quota.Detail)
	q.POST("/batches/:id/reconcile", quota.Reconcile)
	q.POST("/batches/:id/retry", quota.Retry)
	q.POST("/batches/:id/acknowledge", quota.Acknowledge)
	a := group.Group("/third-party-prompt-audit")
	a.GET("/config", handler.GetConfig)
	a.PUT("/config", handler.UpdateConfig)
	a.POST("/test-email", handler.TestAdminEmail)
	a.GET("/contract", handler.GetContract)
	a.POST("/models/list", handler.ListModels)
	a.POST("/models/probe", handler.ProbeModel)
	a.GET("/models/probes/:id", handler.ProbeDetails)
	a.GET("/users/:id/api-keys", handler.UserKeys)
	a.GET("/users", handler.ListAuditUsers)
	a.GET("/groups", handler.ListGroups)
	a.GET("/runtime", handler.GetRuntime)
	a.GET("/stats", handler.GetStats)
	a.GET("/jobs", handler.ListJobs)
	a.GET("/jobs/:id", handler.GetJob)
	a.GET("/jobs/:id/prompts", handler.GetJobPrompts)
	a.GET("/jobs/export", handler.ExportJobs)
	a.POST("/reaudits/preview", handler.PreviewReaudits)
	a.POST("/reaudits", handler.CreateReaudits)
	a.GET("/trust", handler.ListTrust)
	a.POST("/trust", handler.TrustInstruction)
	a.POST("/trust/batch", handler.TrustBatch)
	a.DELETE("/trust/:hash", handler.UntrustInstruction)
	a.GET("/breakers", handler.ListBreakers)
	a.POST("/breakers/drill", handler.DrillBreaker)
	a.POST("/breakers/:hash/confirm", handler.ConfirmBreaker)
	a.POST("/actions/:id/retry", handler.RetryAction)
	a.POST("/users/:id/enable-and-reset", handler.EnableAndReset)
	r.GET("/health", updates.Health)
	r.GET("/enhance/api/v1/health", updates.Health)
	static := http.StripPrefix("/enhance/", http.FileServer(http.FS(assets)))
	r.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/enhance/api/") {
			c.AbortWithStatus(404)
			return
		}
		if strings.HasPrefix(path, "/enhance/") {
			c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'self'; base-uri 'self'; form-action 'self'")
			if strings.HasPrefix(path, "/enhance/assets/") {
				static.ServeHTTP(c.Writer, c.Request)
				return
			}
			raw, err := fs.ReadFile(assets, "index.html")
			if err != nil {
				c.String(503, "增强页面尚未构建")
				return
			}
			c.Data(200, "text/html; charset=utf-8", raw)
			return
		}
		// 只转发模型路径；原版后台与管理员 API 不通过此代理绕行。
		if strings.HasPrefix(path, "/api/") || path == "/" {
			c.AbortWithStatus(404)
			return
		}
		proxy.Serve(c.Writer, c.Request, c.ClientIP())
	})
	return r, nil
}
