package thirdpartypromptaudit

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestCreateReauditRequeuesOriginalJob(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT root.id,root.snapshot_status='complete'.*FROM sub2api_enhance.third_party_prompt_audit_jobs root`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "ready", "active_id"}).AddRow(int64(7), true, nil))
	mock.ExpectQuery(`UPDATE sub2api_enhance.third_party_prompt_audit_jobs root SET.*audit_round=audit_round\+1.*RETURNING root.id`).
		WithArgs(int64(42), int64(9), sqlmock.AnyArg(), MaxEvaluationAttempts, int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))

	result, err := NewRepository(db).CreateReaudits(context.Background(), ReauditRequest{Source: "jobs"}, ConfigSnapshot{Revision: 9}, 42)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Matched)
	require.EqualValues(t, 1, result.Ready)
	require.Equal(t, "requeued", result.Items[0].Status)
	require.EqualValues(t, 7, *result.Items[0].JobID)
	require.NoError(t, mock.ExpectationsWereMet())
}
