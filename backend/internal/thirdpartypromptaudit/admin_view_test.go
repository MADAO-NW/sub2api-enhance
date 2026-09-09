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
		{ModelID: "a", Basis: "segments_all_pass", Segments: []SegmentUse{{Result: SegmentResult{Score: Score{Confidence: .92}}}, {Result: SegmentResult{Score: Score{Confidence: .95}}}}},
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
}

func TestEventListExplainsLatestOutcomeWithItsOwnJobRevision(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(`SELECT COUNT\(\*\).*LEFT JOIN sub2api_enhance.third_party_prompt_audit_jobs original ON original.id=j.source_job_id LEFT JOIN sub2api_enhance.captures capture ON capture.id=COALESCE\(j.capture_id,original.capture_id\).*JOIN sub2api_enhance.third_party_prompt_audit_outcomes o ON o.id=e.latest_outcome_id`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	now := time.Now()
	columns := []string{"id", "job_id", "original", "latest", "created", "updated", "job", "decision", "partial", "outcome_created", "reaudit_status", "models", "decision_config", "audit_round", "duration_ms", "outcome_job_id"}
	rows := sqlmock.NewRows(columns).AddRow(1, 4, 10, 11, now, now,
		`{"id":4,"decision_config":{"revision":3,"review_threshold":1,"block_threshold":1}}`,
		"review", false, now, "done",
		`[{"model_id":"a","basis":"joint","confidence":0.65,"max_segment_confidence":0.95,"reused":true,"joint_attempt_id":8}]`,
		`{"revision":8,"review_threshold":0.5,"block_threshold":0.8}`, 2, int64(125), 9)
	mock.ExpectQuery(`SELECT e.id.*capture.created_at.*original.capture_id.*LEFT JOIN sub2api_enhance.third_party_prompt_audit_jobs original ON original.id=j.source_job_id LEFT JOIN sub2api_enhance.captures capture ON capture.id=COALESCE\(j.capture_id,original.capture_id\).*JOIN sub2api_enhance.third_party_prompt_audit_outcomes o ON o.id=e.latest_outcome_id`).WithArgs(20, 0).WillReturnRows(rows)
	page, err := NewRepository(db).ListEvents(context.Background(), Filter{}, 1, 20)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	event := page.Items[0]
	require.Equal(t, int64(3), event.Job.DecisionConfig.Revision)
	require.Equal(t, int64(8), event.Latest.DecisionConfig.Revision)
	require.Equal(t, int64(9), event.Latest.JobID)
	require.Equal(t, 2, event.Latest.AuditRound)
	require.EqualValues(t, 125, *event.Latest.DurationMS)
	require.Equal(t, .5, *event.Latest.DecisionConfig.ReviewThreshold)
	require.Equal(t, .95, *event.Latest.Models[0].MaxSegmentConfidence)
	require.True(t, event.Latest.Models[0].Reused)
	require.Nil(t, event.Job.FullInput)
	require.Empty(t, event.Job.Config.AuditPrompt)
	require.Empty(t, event.Latest.Models[0].Segments)
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
