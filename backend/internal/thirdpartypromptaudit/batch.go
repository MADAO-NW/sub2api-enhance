package thirdpartypromptaudit

import "time"

type BatchType string

const (
	BatchPendingReview   BatchType = "pending_review"
	BatchFailedRecovery  BatchType = "failed_recovery"
	BatchReauditSelected BatchType = "reaudit_selected"
	BatchReauditFilter   BatchType = "reaudit_filter"
)

type Batch struct {
	ID          int64      `json:"id"`
	Type        BatchType  `json:"batch_type"`
	RequestedBy int64      `json:"requested_by"`
	Status      string     `json:"status"`
	Matched     int64      `json:"matched"`
	Ready       int64      `json:"ready"`
	Processed   int64      `json:"processed"`
	Created     int64      `json:"created"`
	Requeued    int64      `json:"requeued"`
	Resumed     int64      `json:"resumed"`
	Skipped     int64      `json:"skipped"`
	Failed      int64      `json:"failed"`
	LastError   string     `json:"last_error,omitempty"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	CreatedAt   *time.Time `json:"created_at"`
	UpdatedAt   *time.Time `json:"updated_at"`
}
type BatchRequest struct {
	Type      BatchType `json:"batch_type"`
	Filter    Filter    `json:"filter"`
	IDs       []int64   `json:"ids"`
	ReuseMode string    `json:"reuse_mode"`
}
type BatchItem struct {
	ID               int64      `json:"id"`
	BatchID          int64      `json:"batch_id"`
	CaptureID        *int64     `json:"capture_id,omitempty"`
	JobID            *int64     `json:"job_id,omitempty"`
	SourceAuditRound *int       `json:"source_audit_round,omitempty"`
	Status           string     `json:"status"`
	Reason           string     `json:"reason,omitempty"`
	ResultJobID      *int64     `json:"result_job_id,omitempty"`
	ProcessedAt      *time.Time `json:"processed_at,omitempty"`
}
type batchExecution struct {
	Batch
	Snapshot        BatchRequest
	Cursor          int64
	ClaimGeneration int64
}
