package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strings"
	"time"
)

// files 只包含增强迁移；执行流程适配自 sub2api 的 repository/migrations_runner.go。
//
//go:embed *.sql
var files embed.FS

// migrationLock 与原版锁隔离，同库多实例共享增强迁移锁。
const migrationLock int64 = 748239516309711

// migrationLockRetryInterval 沿用原版迁移锁轮询间隔。
const migrationLockRetryInterval = 500 * time.Millisecond

func Run(ctx context.Context, db *sql.DB) error { return runFS(ctx, db, files) }

// runFS 沿用原版的专用连接、逐文件事务、排序与校验和校验，不复制原版历史 Atlas 台账及特例。
func runFS(ctx context.Context, db *sql.DB, source fs.FS) error {
	if db == nil {
		return errors.New("迁移数据库连接未提供")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	ticker := time.NewTicker(migrationLockRetryInterval)
	defer ticker.Stop()
	for {
		var locked bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, migrationLock).Scan(&locked); err != nil {
			return err
		}
		if locked {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var released bool
		if err := conn.QueryRowContext(cleanup, `SELECT pg_advisory_unlock($1)`, migrationLock).Scan(&released); err != nil || !released {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if _, err := conn.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS sub2api_enhance; CREATE TABLE IF NOT EXISTS sub2api_enhance.schema_migrations(filename TEXT PRIMARY KEY,checksum TEXT NOT NULL,applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		return err
	}
	names, err := fs.Glob(source, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		raw, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		content := strings.TrimSpace(string(raw))
		if content == "" {
			continue
		}
		sum := sha256.Sum256([]byte(content))
		checksum := hex.EncodeToString(sum[:])
		var saved string
		err = conn.QueryRowContext(ctx, `SELECT checksum FROM sub2api_enhance.schema_migrations WHERE filename=$1`, name).Scan(&saved)
		if err == nil {
			if saved != checksum {
				return fmt.Errorf("增强迁移内容发生变化：%s；已发布 SQL 不可修改，请新增迁移", name)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		nonTx, err := validateMigrationExecutionMode(name, content)
		if err != nil {
			return fmt.Errorf("迁移 %s 模式无效：%w", name, err)
		}
		log.Printf("开始应用增强迁移 filename=%s checksum=%s", name, checksum)
		if nonTx {
			for _, statement := range splitSQLStatements(content) {
				if stripSQLLineComment(statement) == "" {
					continue
				}
				if _, err := conn.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("非事务迁移 %s 失败：%w", name, err)
				}
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO sub2api_enhance.schema_migrations(filename,checksum) VALUES($1,$2)`, name, checksum); err != nil {
				return err
			}
		} else {
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, content); err != nil {
				tx.Rollback()
				return fmt.Errorf("增强迁移 %s 执行失败：%w", name, err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO sub2api_enhance.schema_migrations(filename,checksum) VALUES($1,$2)`, name, checksum); err != nil {
				tx.Rollback()
				return err
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("增强迁移 %s 提交未确认：%w", name, err)
			}
		}
		log.Printf("增强迁移已完成 filename=%s checksum=%s", name, checksum)
	}
	return nil
}

// Digest 提供嵌入迁移集合的指纹，二进制回退仅允许相同迁移集合，避免自动降级数据库。
func Digest() (string, error) {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		raw, err := files.ReadFile(name)
		if err != nil {
			return "", err
		}
		content := strings.TrimSpace(string(raw))
		if content == "" {
			continue
		}
		sum := sha256.Sum256([]byte(content))
		fmt.Fprintf(h, "%s:%x\n", name, sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
