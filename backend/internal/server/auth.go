package server

import (
	"crypto/rand"
	"encoding/base64"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/url"
	"sub2api-enhance/internal/server/middleware"
	"sub2api-enhance/internal/sub2api"
	"sync"
	"time"
)

// sessionLifetime 为增强会话设置短期上限，不延长原版 token 有效期。
const sessionLifetime = 15 * time.Minute

// sessionCookie 仅用于 /enhance/，不覆盖原后台 Cookie。
const sessionCookie = "sub2api_enhance_session"

type session struct {
	token, ip, ua string
	expires       time.Time
	userID        int64
}
type Auth struct {
	client   *sub2api.Client
	origin   string
	mu       sync.Mutex
	sessions map[string]session
}

func NewAuth(client *sub2api.Client, origin string) *Auth {
	return &Auth{client: client, origin: origin, sessions: make(map[string]session)}
}
func (a *Auth) trustedWrite(c *gin.Context) bool {
	if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
		return true
	}
	return c.GetHeader("Origin") == a.origin && c.GetHeader("X-Enhance-Request") == "1"
}
func (a *Auth) Bootstrap(c *gin.Context) {
	if !a.trustedWrite(c) {
		c.AbortWithStatusJSON(403, gin.H{"message": "请求来源不可信"})
		return
	}
	var input struct {
		Token string `json:"token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.AbortWithStatusJSON(400, gin.H{"message": "缺少原版登录凭据"})
		return
	}
	user, expiry, err := a.client.VerifyAdmin(c.Request.Context(), input.Token, c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		c.AbortWithStatusJSON(401, gin.H{"message": "原版管理员身份验证失败，请重新打开菜单"})
		return
	}
	if limit := time.Now().Add(sessionLifetime); expiry.After(limit) {
		expiry = limit
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		c.AbortWithStatus(503)
		return
	}
	id := base64.RawURLEncoding.EncodeToString(random)
	a.mu.Lock()
	for key, value := range a.sessions {
		if !value.expires.After(time.Now()) {
			delete(a.sessions, key)
		}
	}
	a.sessions[id] = session{token: input.Token, ip: c.ClientIP(), ua: c.Request.UserAgent(), expires: expiry, userID: user.ID}
	a.mu.Unlock()
	parsed, _ := url.Parse(a.origin)
	http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Value: id, Path: "/enhance/", Expires: expiry, HttpOnly: true, Secure: parsed.Scheme == "https", SameSite: http.SameSiteStrictMode})
	c.JSON(200, gin.H{"code": 0, "data": gin.H{"user_id": user.ID, "expires_at": expiry}})
}
func (a *Auth) Require(c *gin.Context) {
	if !a.trustedWrite(c) {
		c.AbortWithStatusJSON(403, gin.H{"message": "请求来源不可信"})
		return
	}
	id, _ := c.Cookie(sessionCookie)
	a.mu.Lock()
	value, ok := a.sessions[id]
	a.mu.Unlock()
	if !ok || !value.expires.After(time.Now()) || value.ip != c.ClientIP() || value.ua != c.Request.UserAgent() {
		c.AbortWithStatusJSON(401, gin.H{"code": "enhance_session_expired", "message": "增强连接已过期，正在尝试重新连接"})
		return
	}
	user, _, err := a.client.VerifyAdmin(c.Request.Context(), value.token, value.ip, value.ua)
	if err != nil || user.ID != value.userID {
		a.mu.Lock()
		delete(a.sessions, id)
		a.mu.Unlock()
		c.AbortWithStatusJSON(401, gin.H{"message": "原版会话不可验证或权限已失效"})
		return
	}
	middleware.SetAuthSubject(c, user.ID)
	c.Next()
}
