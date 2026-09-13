package thirdpartypromptaudit

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRecoverBatchesReturnsProcessingToQueue(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectExec("UPDATE sub2api_enhance.third_party_prompt_audit_batches SET status='queued'").WillReturnResult(sqlmock.NewResult(0, 2))
	require.NoError(t, NewRepository(db).RecoverBatches(context.Background()))
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestGetBatchItemsKeepsFailureDetails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT id,batch_id,capture_id,job_id,source_audit_round,status").WithArgs(int64(7), int64(0), 50).WillReturnRows(sqlmock.NewRows([]string{"id", "batch_id", "capture_id", "job_id", "source_audit_round", "status", "reason", "result_job_id", "processed_at"}).AddRow(int64(1), int64(7), int64(3), nil, nil, "failed", "正文解析失败", nil, nil).AddRow(int64(2), int64(7), nil, int64(9), nil, "requeued", "", nil, nil))
	items, err := NewRepository(db).GetBatchItems(context.Background(), 7, 0, 0)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, "failed", items[0].Status)
	require.Equal(t, "正文解析失败", items[0].Reason)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBatchRequestIntersectsLegacyIDsWithFilterIDs(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO sub2api_enhance.third_party_prompt_audit_batches").WithArgs(BatchReauditSelected, int64(7), sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"id", "batch_type", "requested_by", "status", "matched_count", "ready_count", "created_count", "requeued_count", "resumed_count", "skipped_count", "failed_count", "created_at", "updated_at"}).AddRow(int64(3), string(BatchReauditSelected), int64(7), "queued", 0, 0, 0, 0, 0, 0, 0, nil, nil))
	mock.ExpectQuery("SELECT j.id,j.audit_round").WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"id", "audit_round", "ready", "active"}))
	mock.ExpectExec("UPDATE sub2api_enhance.third_party_prompt_audit_batches SET matched_count").WithArgs(int64(3), int64(0), int64(0)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	_, err = NewRepository(db).CreateBatch(context.Background(), BatchRequest{Type: BatchReauditSelected, IDs: []int64{2, 3}, Filter: Filter{IDs: []int64{3, 4}}}, 7)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
