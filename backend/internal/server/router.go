package server

import (
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
	q.GET("/reset-records", quota.Records)
	q.GET("/reset-records/:id", quota.Detail)
	q.POST("/reset-records/:id/repair-cache", quota.RepairCache)
	q.POST("/deliveries/:id/reconcile", quota.Reconcile)
	a := group.Group("/third-party-prompt-audit")
	a.GET("/config", handler.GetConfig)
	a.PUT("/config", handler.UpdateConfig)
	a.GET("/contract", handler.GetContract)
	a.POST("/models/list", handler.ListModels)
	a.POST("/models/probe", handler.ProbeModel)
	a.GET("/models/probes/:id", handler.ProbeDetails)
	a.GET("/users/:id/api-keys", handler.UserKeys)
	a.GET("/users", handler.ListAuditUsers)
	a.GET("/runtime", handler.GetRuntime)
	a.GET("/stats", handler.GetStats)
	a.GET("/jobs", handler.ListJobs)
	a.GET("/jobs/:id", handler.GetJob)
	a.GET("/jobs/:id/latest-user-content", handler.GetJobLatestUserContent)
	a.POST("/jobs/:id/resume", handler.ResumeResult)
	a.POST("/reaudits/preview", handler.PreviewReaudits)
	a.POST("/reaudits", handler.CreateReaudits)
	a.POST("/recoveries/preview", handler.PreviewRecoveries)
	a.POST("/recoveries", handler.CreateRecoveries)
	a.POST("/actions/:id/retry", handler.RetryAction)
	a.GET("/captures", handler.ListCaptures)
	a.GET("/captures/:id", handler.GetCapture)
	a.GET("/captures/:id/latest-user-content", handler.GetCaptureLatestUserContent)
	a.GET("/captures/:id/raw", handler.DownloadCapture)
	a.POST("/captures/:id/reprocess", handler.ReprocessCapture)
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
