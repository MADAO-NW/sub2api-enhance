package response

import (
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	infraerrors "sub2api-enhance/internal/pkg/errors"
)

// internalErrorReason 标记未分类的内部错误，便于日志检索与前端识别。
const internalErrorReason = "enhance_internal_error"

type Response struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
	Data    any    `json:"data,omitempty"`
}

func Success(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Response{Data: data, Message: "success"})
}
func BadRequest(c *gin.Context, message string) {
	c.AbortWithStatusJSON(400, Response{Code: 400, Message: message})
}

// ErrorFrom 把错误写入响应。业务错误按自身状态返回；未分类错误此前只输出 internal error，
// 页面和日志都看不到原因。管理后台需要直接看见底层失败（例如数据库账号缺少表权限）才能自行修复，
// 因此这里记录完整错误，并把原始错误文本回显到响应。
func ErrorFrom(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	var appErr *infraerrors.ApplicationError
	if !errors.As(err, &appErr) {
		if c.Request != nil {
			log.Printf("增强服务请求处理失败 method=%s path=%s error_type=%T error=%v", c.Request.Method, c.Request.URL.Path, err, err)
		} else {
			log.Printf("增强服务请求处理失败 error_type=%T error=%v", err, err)
		}
		appErr = infraerrors.InternalServer(internalErrorReason, "服务内部错误："+err.Error()).WithCause(err)
	}
	code, status := infraerrors.ToHTTP(appErr)
	c.AbortWithStatusJSON(code, Response{Code: code, Message: status.Message, Reason: status.Reason})
	return true
}

func Accepted(c *gin.Context, data any) {
	c.JSON(http.StatusAccepted, Response{Data: data, Message: "accepted"})
}
