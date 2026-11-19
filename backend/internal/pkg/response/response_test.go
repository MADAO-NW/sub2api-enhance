package response

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	infraerrors "sub2api-enhance/internal/pkg/errors"
)

// newErrorContext 构造一个带请求上下文的测试响应记录器，用于校验错误回显内容。
func newErrorContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/enhance/api/v1/admin/quota-follow/discovery", nil)
	return ctx, recorder
}

// TestErrorFromEchoesUnclassifiedCause 覆盖数据库等未分类错误：页面必须看到原始原因，而不是 internal error。
func TestErrorFromEchoesUnclassifiedCause(t *testing.T) {
	ctx, recorder := newErrorContext()
	require.True(t, ErrorFrom(ctx, errors.New("permission denied for table users")))
	require.Equal(t, http.StatusInternalServerError, recorder.Code)

	var body Response
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, http.StatusInternalServerError, body.Code)
	require.Equal(t, internalErrorReason, body.Reason)
	require.Contains(t, body.Message, "permission denied for table users")
}

// TestErrorFromKeepsApplicationError 保证已分类的业务错误仍按原状态和原消息返回，不被包装改写。
func TestErrorFromKeepsApplicationError(t *testing.T) {
	ctx, recorder := newErrorContext()
	require.True(t, ErrorFrom(ctx, infraerrors.BadRequest("quota_follow_monitor_invalid", "监控账号不可用").WithCause(errors.New("底层细节"))))
	require.Equal(t, http.StatusBadRequest, recorder.Code)

	var body Response
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "监控账号不可用", body.Message)
	require.Equal(t, "quota_follow_monitor_invalid", body.Reason)
}

// TestErrorFromIgnoresNil 保证没有错误时不写响应，避免覆盖正常结果。
func TestErrorFromIgnoresNil(t *testing.T) {
	ctx, recorder := newErrorContext()
	require.False(t, ErrorFrom(ctx, nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Empty(t, recorder.Body.String())
}
