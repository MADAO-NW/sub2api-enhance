package quotafollow

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
	"testing"
)

type immediateResetSource struct {
	users []sub2api.QuotaUser
}

func (s immediateResetSource) Discover(context.Context, int64) (sub2api.QuotaDiscovery, error) {
	return sub2api.QuotaDiscovery{Users: s.users}, nil
}
func (immediateResetSource) Accounts(context.Context, int64) ([]sub2api.QuotaAccount, error) {
	return nil, nil
}
func (immediateResetSource) Snapshot(context.Context, int64, int64) (*sub2api.QuotaSnapshot, error) {
	return nil, nil
}

type immediateResetAPI struct {
	calls []string
}

func (immediateResetAPI) Configured() bool { return true }
func (immediateResetAPI) AccountUsageBatch(context.Context, []int64) (map[int64]sub2api.AccountUsage, error) {
	return nil, nil
}
func (a *immediateResetAPI) ResetQuota(_ context.Context, userID int64, window, requestID string) sub2api.QuotaResetReply {
	a.calls = append(a.calls, requestID+":"+window)
	if userID == 2 {
		return sub2api.QuotaResetReply{HTTPStatus: 500, Error: errors.New("上游失败")}
	}
	return sub2api.QuotaResetReply{HTTPStatus: 200}
}

func TestImmediateResetCallsOriginalAPIForEachSelectedUserAndWindow(t *testing.T) {
	api := &immediateResetAPI{}
	service := &Service{api: api, source: immediateResetSource{users: []sub2api.QuotaUser{{ID: 1, Username: "one"}, {ID: 2, Username: "two"}}}}
	result, err := service.ImmediateReset(context.Background(), ImmediateResetRequest{GroupID: 7, Windows: []string{"weekly", "daily", "weekly"}})
	require.NoError(t, err)
	require.Equal(t, int64(7), result.GroupID)
	require.Equal(t, []string{"weekly", "daily"}, result.Windows)
	require.Equal(t, 4, result.Total)
	require.Equal(t, 2, result.Succeeded)
	require.Equal(t, 2, result.NonSuccess)
	require.Len(t, api.calls, 4)
	require.Equal(t, "succeeded", result.Items[0].Status)
	require.Equal(t, "uncertain", result.Items[2].Status)
	require.NotEmpty(t, result.Items[0].RequestID)
}

func TestImmediateResetRejectsEmptyOrInvalidWindow(t *testing.T) {
	service := &Service{api: &immediateResetAPI{}, source: immediateResetSource{}}
	_, err := service.ImmediateReset(context.Background(), ImmediateResetRequest{GroupID: 7})
	require.ErrorIs(t, err, errImmediateResetNoWindow)
	_, err = service.ImmediateReset(context.Background(), ImmediateResetRequest{GroupID: 7, Windows: []string{"monthly"}})
	require.ErrorIs(t, err, errImmediateResetWindow)
}
