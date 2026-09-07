package server

import (
	"encoding/base64"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"sub2api-enhance/internal/config"
	"sub2api-enhance/internal/sub2api"
	"testing"
	"time"
)

func TestSessionRequiresTrustedOriginAndRevalidatesAdmin(t *testing.T) {
	active := true
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix()))) + ".signature"
	original := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !active {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"id":7,"role":"admin","status":"active"}}`)
	}))
	defer original.Close()
	client := sub2api.NewClient(&config.Config{OfficialURL: original.URL})
	auth := NewAuth(client, "https://enhance.example")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	require.NoError(t, r.SetTrustedProxies(nil))
	r.POST("/bootstrap", auth.Bootstrap)
	r.POST("/protected", auth.Require, func(c *gin.Context) { c.Status(204) })
	request := func(path string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"token":"`+token+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		req.Header.Set("X-Enhance-Request", "1")
		req.Header.Set("User-Agent", "unit-test")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		return out
	}
	require.Equal(t, 403, request("/bootstrap", nil, "https://untrusted.example").Code)
	logged := request("/bootstrap", nil, "https://enhance.example")
	require.Equal(t, 200, logged.Code)
	cookies := logged.Result().Cookies()
	require.Len(t, cookies, 1)
	require.True(t, cookies[0].HttpOnly)
	require.True(t, cookies[0].Secure)
	require.Equal(t, "/enhance/", cookies[0].Path)
	require.NotContains(t, cookies[0].Value, token)
	require.Equal(t, 204, request("/protected", cookies[0], "https://enhance.example").Code)
	require.Equal(t, 403, request("/protected", cookies[0], "https://untrusted.example").Code)
	active = false
	require.Equal(t, 401, request("/protected", cookies[0], "https://enhance.example").Code)
}
