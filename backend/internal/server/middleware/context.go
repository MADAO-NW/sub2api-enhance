package middleware

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"log"
)

// actorKey 仅由已验证管理员会话设置，禁止从请求参数接受身份。
const actorKey = "enhance.admin"

type AuthSubject struct{ UserID int64 }

func SetAuthSubject(c *gin.Context, id int64) { c.Set(actorKey, AuthSubject{UserID: id}) }
func GetAuthSubjectFromContext(c *gin.Context) (AuthSubject, bool) {
	v, ok := c.Get(actorKey)
	if !ok {
		return AuthSubject{}, false
	}
	s, ok := v.(AuthSubject)
	return s, ok
}
func SetAuditExtra(c *gin.Context, value any) {
	raw, err := json.Marshal(value)
	if err == nil {
		s, _ := GetAuthSubjectFromContext(c)
		log.Printf("管理员审核操作完成 actor_user_id=%d path=%s result=%s", s.UserID, c.Request.URL.Path, raw)
	}
}
