package thirdpartypromptaudit

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRecoveryPreviewSeparatesCheckpointAndExcludedUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT c.id,j.id,c.user_id`).WillReturnRows(sqlmock.NewRows([]string{"capture_id", "job_id", "user_id", "checkpoint"}).
		AddRow(int64(3), int64(8), int64(5), true).
		AddRow(int64(4), int64(9), int64(6), true))
	config := testConfig()
	config.ExcludedUserIDs = []int64{6}
	service := &Service{repo: NewRepository(db), config: &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: config, Revision: 7}}}}
	result, err := service.PreviewRecoveries(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 2, result.Matched)
	require.EqualValues(t, 1, result.Ready)
	require.Equal(t, "resume_checkpoint", result.Items[0].Action)
	require.Equal(t, "ready", result.Items[0].Status)
	require.Equal(t, "skipped", result.Items[1].Status)
	require.Contains(t, result.Items[1].Reason, "不审核")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRecoverySubmissionResumesCheckpointWithoutNewAuditRound(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT c.id,j.id,c.user_id`).WillReturnRows(sqlmock.NewRows([]string{"capture_id", "job_id", "user_id", "checkpoint"}).
		AddRow(int64(3), int64(8), int64(5), true))
	mock.ExpectExec(`UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET status='retry'`).WithArgs(int64(8)).WillReturnResult(sqlmock.NewResult(0, 1))
	service := &Service{repo: NewRepository(db), config: &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: testConfig(), Revision: 7}}}}
	result, err := service.CreateRecoveries(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, "resumed", result.Items[0].Status)
	require.Equal(t, "resume_checkpoint", result.Items[0].Action)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestModelNameFilterUsesLatestOutcomeAndCurrentFailedRound(t *testing.T) {
	where, args := filterSQL(Filter{ModelName: "flash"}, true)
	require.Len(t, args, 1)
	require.Equal(t, "flash", args[0])
	require.Contains(t, where, "model->>'model_name' ILIKE")
	require.Contains(t, where, "attempt.audit_round=j.audit_round")
	require.Contains(t, where, "j.status='failed'")
}

func TestPaginationAcceptsTwoHundredRows(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?page=3&page_size=200", nil)
	page, size, err := paginationQuery(c)
	require.NoError(t, err)
	require.Equal(t, 3, page)
	require.Equal(t, 200, size)
	c, _ = gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?page_size=201", nil)
	_, _, err = paginationQuery(c)
	require.Error(t, err)
}
