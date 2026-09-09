package thirdpartypromptaudit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOutcomeExplanationDoesNotMutateStoredScores(t *testing.T) {
	outcome := &Outcome{JobID: 4, Evaluation: Evaluation{Decision: DecisionPass, Models: []ModelResult{
		{ModelID: "a", Basis: "segments_all_pass", Segments: []SegmentUse{{Result: SegmentResult{UserID: 7, SourceAttemptID: 91, Score: Score{Confidence: .92}}}, {Result: SegmentResult{UserID: 7, SourceAttemptID: 92, Score: Score{Confidence: .95}}}}},
		{ModelID: "b", Error: &AuditError{Code: "timeout"}},
	}}}
	threshold := 1.0
	view := outcomeView(outcome, &DecisionConfig{Revision: 3, ReviewThreshold: &threshold, BlockThreshold: &threshold})
	require.Equal(t, .95, *view.Models[0].MaxSegmentConfidence)
	require.Nil(t, view.Models[0].Confidence)
	require.Nil(t, view.Models[1].MaxSegmentConfidence)
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	var dto map[string]any
	require.NoError(t, json.Unmarshal(raw, &dto))
	models := dto["models"].([]any)
	require.Len(t, models, 2)
	require.Equal(t, .95, models[0].(map[string]any)["max_segment_confidence"])
	require.Nil(t, models[0].(map[string]any)["confidence"])
	stored, err := json.Marshal(outcome)
	require.NoError(t, err)
	require.NotContains(t, string(stored), "max_segment_confidence")
	require.NotContains(t, string(stored), "decision_config")
	segment, err := json.Marshal(outcome.Models[0].Segments[0].Result)
	require.NoError(t, err)
	require.NotContains(t, string(segment), "user_id")
	require.NotContains(t, string(segment), "source_attempt_id")
}

func TestJobListIncludesLatestOutcomeAndOriginalDecision(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT COUNT\(\*\).*LEFT JOIN LATERAL.*third_party_prompt_audit_outcomes`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	now := time.Now()
	record := `{"id":4,"created_at":"` + now.Format(time.RFC3339Nano) + `","display_username":"当前用户名","display_email":"current@example.invalid","decision_config":{"revision":3,"review_threshold":1,"block_threshold":1},"original_decision":"block","outcome":{"id":11,"job_id":4,"user_id":7,"decision":"review","partial_failure":false,"audit_round":2,"reuse_mode":"allow","duration_ms":125,"models":[{"model_id":"a","basis":"joint","confidence":0.65,"max_segment_confidence":0.95,"reused":true,"joint_attempt_id":8}],"decision_config":{"revision":8,"review_threshold":0.5,"block_threshold":0.8}}}`
	mock.ExpectQuery(`SELECT row_to_json\(record\).*AS outcome.*original_decision`).WithArgs(20, 0).WillReturnRows(sqlmock.NewRows([]string{"record"}).AddRow(record))
	page, err := NewRepository(db).ListJobs(context.Background(), Filter{}, 1, 20)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	job := page.Items[0]
	require.Equal(t, int64(3), job.DecisionConfig.Revision)
	require.Equal(t, "当前用户名", job.DisplayUsername)
	require.Equal(t, "current@example.invalid", job.DisplayEmail)
	require.Equal(t, DecisionBlock, *job.OriginalDecision)
	require.Equal(t, int64(8), job.Outcome.DecisionConfig.Revision)
	require.Equal(t, int64(4), job.Outcome.JobID)
	require.Equal(t, 2, job.Outcome.AuditRound)
	require.EqualValues(t, 125, *job.Outcome.DurationMS)
	require.Equal(t, .5, *job.Outcome.DecisionConfig.ReviewThreshold)
	require.Equal(t, .95, *job.Outcome.Models[0].MaxSegmentConfidence)
	require.True(t, job.Outcome.Models[0].Reused)
	require.Nil(t, job.FullInput)
	require.Empty(t, job.Config.AuditPrompt)
	require.Empty(t, job.Outcome.Models[0].Segments)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestJobListDecisionAndModelFiltersUseOnlyLatestOutcome(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT COUNT\(\*\).*o.decision=\$1.*json_array_elements\(o.model_results::json\)`).
		WithArgs(DecisionBlock, "node-a").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT row_to_json\(record\).*o.decision=\$1.*json_array_elements\(o.model_results::json\)`).
		WithArgs(DecisionBlock, "node-a", 20, 0).WillReturnRows(sqlmock.NewRows([]string{"record"}))
	page, err := NewRepository(db).ListJobs(context.Background(), Filter{Decision: DecisionBlock, ModelID: "node-a"}, 1, 20)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestJobDetailDecodePreservesHistoricalPolicyAndThreshold(t *testing.T) {
	config := `{"revision":3,"review_threshold":1,"block_threshold":1,"fixed_roles":"历史角色政策","models":[{"id":"a","temperature":0.1,"max_tokens":2048}]}`
	record, err := json.Marshal(map[string]any{"id": 4, "config_snapshot": config, "status": "done"})
	require.NoError(t, err)
	job, err := decodeJob(record)
	require.NoError(t, err)
	require.Equal(t, int64(3), job.DecisionConfig.Revision)
	raw, err := json.Marshal(job.Config)
	require.NoError(t, err)
	require.JSONEq(t, config, string(raw))
}
