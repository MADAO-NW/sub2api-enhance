package response

import (
	"github.com/gin-gonic/gin"
	"net/http"
	infraerrors "sub2api-enhance/internal/pkg/errors"
)

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
func ErrorFrom(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	code, status := infraerrors.ToHTTP(err)
	c.AbortWithStatusJSON(code, Response{Code: code, Message: status.Message, Reason: status.Reason})
	return true
}
func Accepted(c *gin.Context, data any) {
	c.JSON(http.StatusAccepted, Response{Data: data, Message: "accepted"})
}
