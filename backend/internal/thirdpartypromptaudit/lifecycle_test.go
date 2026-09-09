package thirdpartypromptaudit

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestPublicSavedConfigSurvivesDecryptionFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	stored := storedConfig{Config: testConfig(), Revision: 7, EncryptedKeys: map[string]string{"test-node": "encrypted-value"}}
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WithArgs(SettingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(raw)))
	mock.ExpectQuery("SELECT key,value FROM sub2api_enhance.settings").WithArgs(SettingKey).WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow(SettingKey, string(raw)))
	manager := &ConfigManager{db: db}
	saved, err := manager.ReadSaved(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(7), saved.Revision)
	require.True(t, saved.HasAPIKeys["test-node"])
	require.NotEmpty(t, saved.ApplicationError)
	exposed, err := json.Marshal(saved)
	require.NoError(t, err)
	require.NotContains(t, string(exposed), "encrypted-value")
	require.Equal(t, "async", manager.EffectiveMode())
	_, err = manager.Active()
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestReloadFailureKeepsActualCapacityAndConfiguredMode(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	manager := &ConfigManager{db: db, active: &activeConfig{Stored: storedConfig{Config: testConfig(), Revision: 5}}}
	broken := storedConfig{Config: testConfig(), Revision: 6}
	broken.WorkerCount = 0
	raw, err := json.Marshal(broken)
	require.NoError(t, err)
	mock.ExpectQuery("SELECT key,value FROM sub2api_enhance.settings").WithArgs(SettingKey).WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow(SettingKey, string(raw)))
	require.Error(t, manager.Reload(context.Background()))
	require.Equal(t, "async", manager.EffectiveMode())
	require.Equal(t, 4, manager.WorkerCapacity())
	require.Equal(t, int64(5), manager.active.Stored.Revision)
	require.Equal(t, int64(6), manager.expectedRevision)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestReloadSerializesReadAndPublish(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	manager := &ConfigManager{db: db}
	first, _ := json.Marshal(storedConfig{Config: testConfig(), Revision: 1})
	second, _ := json.Marshal(storedConfig{Config: testConfig(), Revision: 2})
	mock.ExpectQuery("SELECT key,value FROM sub2api_enhance.settings").WillDelayFor(40 * time.Millisecond).WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow(SettingKey, string(first)))
	mock.ExpectQuery("SELECT key,value FROM sub2api_enhance.settings").WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow(SettingKey, string(second)))
	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() { defer wg.Done(); require.NoError(t, manager.Reload(context.Background())) }()
	}
	wg.Wait()
	require.Equal(t, int64(2), manager.active.Stored.Revision)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSaveKeepsStoredCredentialWhenNodeIdentityChangesAndKeyIsBlank(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	current := storedConfig{Config: testConfig(), Revision: 1, EncryptedKeys: map[string]string{"test-node": "encrypted-value"}}
	raw, err := json.Marshal(current)
	require.NoError(t, err)
	next := current.Config
	next.Models[0].BaseURL = "https://new.example.invalid"
	next.Models[0].Model = "new-model"
	mock.ExpectBegin()
	mock.ExpectExec("SELECT pg_advisory_xact_lock").WithArgs(configLockKey).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT value FROM sub2api_enhance.settings").WithArgs(SettingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(raw)))
	mock.ExpectExec("INSERT INTO sub2api_enhance.settings").WithArgs(SettingKey, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT key,value FROM sub2api_enhance.settings").WithArgs(SettingKey).WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow(SettingKey, string(raw)))
	manager := &ConfigManager{db: db}
	saved, err := manager.Save(context.Background(), ConfigUpdate{ExpectedRevision: 1, Config: next, Keys: []KeyUpdate{{ModelID: "test-node", Action: "keep"}}}, 9)
	require.NoError(t, err)
	require.True(t, saved.HasAPIKeys["test-node"])
	require.Equal(t, "https://new.example.invalid", saved.Models[0].BaseURL)
	require.Equal(t, "new-model", saved.Models[0].Model)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTransientLeaseTimeoutRetriesButLostLeaseDoesNotWrite(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "renew timeout", true: "lost ownership"}[lost], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			svc := &Service{repo: NewRepository(db), metrics: NewRuntimeMetrics()}
			job := &Job{ID: 1, Attempts: 1, MaxAttempts: 3, ClaimGeneration: 2, ExecutionMode: "async"}
			ctx, cancel := context.WithCancelCause(context.Background())
			if lost {
				cancel(ErrLeaseLost)
			} else {
				cancel(context.DeadlineExceeded)
			}
			if !lost {
				mock.ExpectBegin()
				mock.ExpectQuery("SELECT claim_generation").WithArgs(int64(1), int64(2)).WillReturnRows(sqlmock.NewRows([]string{"owned"}).AddRow(true))
				mock.ExpectExec("UPDATE sub2api_enhance.third_party_prompt_audit_model_attempts").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("UPDATE sub2api_enhance.third_party_prompt_audit_jobs").WithArgs(int64(1), "retry", "result_persist", "lease_renew_failed", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			}
			failure := svc.finishFailure(ctx, job, requestFailure(context.Canceled))
			if lost {
				require.Equal(t, "lease_lost", failure.Code)
			} else {
				require.True(t, failure.Retryable)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestPersistenceFailuresDoNotRetryPermanentConstraintErrors(t *testing.T) {
	for _, tc := range []struct {
		err   error
		retry bool
	}{
		{&pq.Error{Code: "23514"}, false}, {&pq.Error{Code: "42601"}, false}, {&pq.Error{Code: "40001"}, true}, {&pq.Error{Code: "08006"}, true},
		{driver.ErrBadConn, true}, {context.DeadlineExceeded, true}, {errors.New("invalid stored snapshot"), false},
	} {
		require.Equal(t, tc.retry, persistenceFailure(tc.err, "result_persist_failed").Retryable)
	}
}

func TestSharedEvaluationSlotsRespectConfiguredCapacity(t *testing.T) {
	config := testConfig()
	config.WorkerCount = 4
	svc := &Service{config: &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: config}}}, wake: make(chan struct{}, 1)}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	release := make(chan struct{})
	ready := make(chan struct{}, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if svc.tryAcquireSlot() {
				admitted.Add(1)
				ready <- struct{}{}
				<-release
				svc.releaseSlot()
			} else {
				ready <- struct{}{}
			}
		}()
	}
	for range 20 {
		<-ready
	}
	require.Equal(t, int64(4), admitted.Load())
	require.Equal(t, int64(4), svc.active.Load())
	close(release)
	wg.Wait()
	require.Zero(t, svc.active.Load())
}

func TestConfigRejectsFixedContractAndDuplicateJSONKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{`{"expected_revision":1,"config":{"mode":"off","fixed_contract":"override"}}`, `{"expected_revision":1,"expected_revision":2,"config":{}}`, `{"expected_revision":1,"fixed_roles":"override","config":{}}`} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest("PUT", "/config", strings.NewReader(body))
		var update ConfigUpdate
		require.Error(t, bindStrict(ctx, &update))
	}
}

func TestCompleteIsIdempotentAndFencesExpiredOwners(t *testing.T) {
	for _, done := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired lease", true: "already committed"}[done], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectBegin()
			status := "processing"
			if done {
				status = "done"
			}
			mock.ExpectQuery("SELECT status,claim_generation").WithArgs(int64(9)).WillReturnRows(sqlmock.NewRows([]string{"status", "generation", "valid", "disable_counted"}).AddRow(status, 2, false, false))
			if done {
				mock.ExpectCommit()
				mock.ExpectQuery("SELECT id,job_id,user_id,decision").WithArgs(int64(9)).WillReturnRows(sqlmock.NewRows([]string{"id", "job_id", "user_id", "decision", "partial", "eligible", "models", "source", "created", "audit_round", "run_kind", "requested_by", "config", "started", "finished", "reuse_mode"}).AddRow(4, 9, 1, "block", false, true, "[]", nil, time.Now(), 1, "request", nil, "{}", nil, time.Now(), "allow"))
			} else {
				mock.ExpectRollback()
			}
			outcome, err := NewRepository(db).Complete(context.Background(), &Job{ID: 9, ClaimGeneration: 1}, &Evaluation{Decision: DecisionBlock}, true)
			if done {
				require.NoError(t, err)
				require.Equal(t, int64(4), outcome.ID)
			} else {
				require.ErrorIs(t, err, ErrLeaseLost)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestHistoricalCheckpointDoesNotEnterModelEvaluation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT status,claim_generation").WithArgs(int64(9)).WillReturnRows(sqlmock.NewRows([]string{"status", "generation", "valid", "disable_counted"}).AddRow("done", 2, false, false))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT id,job_id,user_id,decision").WithArgs(int64(9)).WillReturnRows(sqlmock.NewRows([]string{"id", "job_id", "user_id", "decision", "partial", "eligible", "models", "source", "created", "audit_round", "run_kind", "requested_by", "config", "started", "finished", "reuse_mode"}).AddRow(4, 9, 1, "pass", false, false, "[]", nil, time.Now(), 1, "request", nil, "{}", nil, time.Now(), "allow"))
	var config ConfigSnapshot
	require.NoError(t, json.Unmarshal([]byte(`{"fixed_roles":"历史规则"}`), &config))
	job := &Job{ID: 9, Config: config, ExecutionMode: "async", Checkpoint: &Evaluation{Decision: DecisionPass}}
	// 不装配 evaluator；有检查点的恢复路径只完成已有结果提交。
	svc := &Service{repo: NewRepository(db), metrics: NewRuntimeMetrics(), wake: make(chan struct{}, 1)}
	outcome, failure := svc.processJob(context.Background(), job, false)
	require.Nil(t, failure)
	require.Equal(t, int64(4), outcome.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}
