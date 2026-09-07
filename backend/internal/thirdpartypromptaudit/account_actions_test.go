package thirdpartypromptaudit

import (
	"context"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"sub2api-enhance/internal/config"
	"sub2api-enhance/internal/sub2api"
	"sync/atomic"
	"testing"
)

func TestUnknownAccountActionOnlyReadsOriginalStatus(t *testing.T) {
	var writes atomic.Int64
	original := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
		}
		fmt.Fprint(w, `{"code":0,"data":{"id":7,"role":"user","status":"active"}}`)
	}))
	defer original.Close()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectExec("UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_actions SET notification_status").WillReturnResult(sqlmock.NewResult(0, 1))
	svc := &Service{repo: NewRepository(db), accounts: sub2api.NewClient(&config.Config{OfficialURL: original.URL, AdminKey: "unit-test-only"}), metrics: NewRuntimeMetrics()}
	action := &Action{ID: 1, UserID: 7, ActionType: "disable", ExecutionStatus: "unknown", BusinessSnapshot: map[string]any{}, AttemptHistory: []AccountAttempt{{Status: "unknown"}}}
	require.False(t, svc.executeAccountAction(context.Background(), action))
	require.Equal(t, "unknown", action.ExecutionStatus)
	require.Zero(t, writes.Load())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEnableResetLocksActionsAndFencesCancelledWorker(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_states").WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT disable_violation_count .* FOR UPDATE").WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
	mock.ExpectQuery("SELECT action_type,execution_status .* FOR UPDATE").WithArgs(int64(7)).WillReturnRows(sqlmock.NewRows([]string{"action_type", "execution_status"}).AddRow("disable", "pending"))
	mock.ExpectExec("UPDATE .* SET execution_status='cancelled',claim_generation=claim_generation\\+1,lease_until=NULL").WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("INSERT INTO sub2api_enhance.third_party_prompt_audit_enforcement_actions").WillReturnRows(sqlmock.NewRows([]string{"id", "applied_at"}).AddRow(11, nil))
	mock.ExpectCommit()
	id, err := NewRepository(db).CreateCounterReset(context.Background(), 7, 9)
	require.NoError(t, err)
	require.EqualValues(t, 11, id)
	require.NoError(t, mock.ExpectationsWereMet())
}
