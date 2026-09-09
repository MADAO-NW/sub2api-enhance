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
	mock.ExpectQuery(`SELECT j.id,j.user_id,j.snapshot_status='complete'.*FROM sub2api_enhance.third_party_prompt_audit_jobs j`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "ready", "active_id"}).AddRow(int64(7), int64(5), true, nil))
	mock.ExpectQuery(`UPDATE sub2api_enhance.third_party_prompt_audit_jobs root SET.*audit_round=audit_round\+1.*attempts=0.*RETURNING root.id`).
		WithArgs(int64(42), int64(9), sqlmock.AnyArg(), ReuseModeAllow, MaxEvaluationAttempts, int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))

	result, err := NewRepository(db).CreateReaudits(context.Background(), ReauditRequest{}, ConfigSnapshot{Revision: 9}, 42)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Matched)
	require.EqualValues(t, 1, result.Ready)
	require.Equal(t, "requeued", result.Items[0].Status)
	require.EqualValues(t, 7, result.Items[0].JobID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBindEvaluationConfigReplacesOnlyDerivedEvaluationState(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectExec(`UPDATE sub2api_enhance.third_party_prompt_audit_jobs SET config_revision=\$3,config_snapshot=\$4`).
		WithArgs(int64(7), int64(3), int64(9), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	job := &Job{ID: 7, ClaimGeneration: 3, InputHash: "old-input", TargetHash: "old-target", EvaluationHash: "old-evaluation", Manifest: []SegmentMeta{{Order: 1}}}
	snapshot := ConfigSnapshot{Config: testConfig(), Revision: 9, ContractVersion: ContractVersion, FixedContract: OutputContract}
	require.NoError(t, NewRepository(db).BindEvaluationConfig(context.Background(), job, snapshot))
	require.Equal(t, int64(9), job.Config.Revision)
	require.Empty(t, job.Manifest)
	require.Empty(t, job.InputHash)
	require.Empty(t, job.TargetHash)
	require.Empty(t, job.EvaluationHash)
	require.NoError(t, mock.ExpectationsWereMet())
}
