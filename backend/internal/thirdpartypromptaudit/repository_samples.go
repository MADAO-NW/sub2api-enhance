package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/lib/pq"
)

// PolicySample 是管理员标注“应该通过/应该阻断”的测试用例，不改变任务结论。
type PolicySample struct {
	ID          int64     `json:"id"`
	Hash        string    `json:"hash"`
	Kind        string    `json:"kind"`
	Expected    string    `json:"expected"`
	Note        string    `json:"note"`
	SourceJobID *int64    `json:"source_job_id,omitempty"`
	CreatedBy   int64     `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Preview     string    `json:"preview"`
	// SourceUserID 只用于按用户删除，不在列表接口暴露。
	SourceUserID *int64 `json:"-"`
}

// SampleInput 是样本的期望与备注；hash 与 kind 由后端从任务目标读取。
type SampleInput struct {
	JobID       int64  `json:"job_id"`
	TargetOrder int    `json:"target_order"`
	Expected    string `json:"expected" binding:"required,oneof=pass block"`
	Note        string `json:"note" binding:"max=500"`
}

const sampleColumns = `id,hash,kind,expected,note,source_job_id,source_user_id,created_by,created_at,updated_at`

// CreateSample 从任务目标读取 hash 与 kind 并保存样本；同一 hash 已有样本时更新期望与备注。
func (r *Repository) CreateSample(ctx context.Context, input SampleInput, actorID int64) (*PolicySample, error) {
	var hash, kind string
	var userID int64
	err := r.db.QueryRowContext(ctx, `SELECT t.hash,t.kind,j.user_id FROM sub2api_enhance.audit_job_targets t
 JOIN sub2api_enhance.audit_jobs j ON j.id=t.job_id
 WHERE t.job_id=$1 AND t.target_order=$2 AND t.round=j.current_round`, input.JobID, input.TargetOrder).Scan(&hash, &kind, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var sample PolicySample
	var sourceJob sql.NullInt64
	var sourceUser sql.NullInt64
	err = r.db.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.policy_samples(hash,kind,expected,note,source_job_id,source_user_id,created_by)
 VALUES($1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT (hash) DO UPDATE SET expected=EXCLUDED.expected,note=EXCLUDED.note,updated_at=clock_timestamp()
 RETURNING `+sampleColumns, hash, kind, input.Expected, input.Note, input.JobID, userID, actorID).Scan(
		&sample.ID, &sample.Hash, &sample.Kind, &sample.Expected, &sample.Note, &sourceJob, &sourceUser, &sample.CreatedBy, &sample.CreatedAt, &sample.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if sourceJob.Valid {
		sample.SourceJobID = &sourceJob.Int64
	}
	if sourceUser.Valid {
		sample.SourceUserID = &sourceUser.Int64
	}
	return &sample, nil
}

// ListSamples 返回分页样本，可按 kind 与 expected 过滤。
func (r *Repository) ListSamples(ctx context.Context, kind, expected string, page, pageSize int) (*Page[PolicySample], error) {
	if kind != "" && kind != PromptKindInstruction && kind != PromptKindUser {
		return nil, errors.New("样本类型筛选无效")
	}
	if expected != "" && expected != string(DecisionPass) && expected != string(DecisionBlock) {
		return nil, errors.New("样本期望筛选无效")
	}
	result := &Page[PolicySample]{Items: []PolicySample{}, Page: page, PageSize: pageSize}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.policy_samples
 WHERE ($1='' OR kind=$1) AND ($2='' OR expected=$2)`, kind, expected).Scan(&result.Total); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT s.id,s.hash,s.kind,s.expected,s.note,s.source_job_id,s.source_user_id,s.created_by,s.created_at,s.updated_at,COALESCE(left(p.text,500),'')
 FROM sub2api_enhance.policy_samples s LEFT JOIN sub2api_enhance.prompt_texts p ON p.hash=s.hash
 WHERE ($1='' OR s.kind=$1) AND ($2='' OR s.expected=$2)
 ORDER BY s.updated_at DESC,s.id DESC LIMIT $3 OFFSET $4`, kind, expected, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var sample PolicySample
		var sourceJob, sourceUser sql.NullInt64
		if err := rows.Scan(&sample.ID, &sample.Hash, &sample.Kind, &sample.Expected, &sample.Note, &sourceJob, &sourceUser, &sample.CreatedBy, &sample.CreatedAt, &sample.UpdatedAt, &sample.Preview); err != nil {
			return nil, err
		}
		if sourceJob.Valid {
			sample.SourceJobID = &sourceJob.Int64
		}
		if sourceUser.Valid {
			sample.SourceUserID = &sourceUser.Int64
		}
		result.Items = append(result.Items, sample)
	}
	return result, rows.Err()
}

// UpdateSample 修改样本的期望结论与备注。
func (r *Repository) UpdateSample(ctx context.Context, id int64, expected, note string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.policy_samples SET expected=$2,note=$3,updated_at=clock_timestamp() WHERE id=$1`, id, expected, note)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSample 删除一个样本。
func (r *Repository) DeleteSample(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM sub2api_enhance.policy_samples WHERE id=$1`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SampleTexts 读取样本对应的正文，供政策回放使用；正文只来自 prompt_texts。
func (r *Repository) SampleTexts(ctx context.Context, ids []int64) ([]SampleText, error) {
	query := `SELECT s.id,s.hash,s.kind,s.expected,COALESCE(p.text,'') FROM sub2api_enhance.policy_samples s
 LEFT JOIN sub2api_enhance.prompt_texts p ON p.hash=s.hash`
	args := []any{}
	if len(ids) > 0 {
		query += ` WHERE s.id=ANY($1)`
		args = append(args, pq.Array(ids))
	}
	query += ` ORDER BY s.id`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]SampleText, 0)
	for rows.Next() {
		var item SampleText
		if err := rows.Scan(&item.ID, &item.Hash, &item.Kind, &item.Expected, &item.Text); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// SampleText 是回放使用的样本正文。
type SampleText struct {
	ID       int64  `json:"id"`
	Hash     string `json:"hash"`
	Kind     string `json:"kind"`
	Expected string `json:"expected"`
	Text     string `json:"-"`
}

// DeleteSamplesForUser 在删除用户数据前先删除该用户的样本，避免外键阻塞文本清理。
func (r *Repository) DeleteSamplesForUser(ctx context.Context, tx *sql.Tx, userID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM sub2api_enhance.policy_samples WHERE source_user_id=$1`, userID)
	return err
}

// PolicyReplayResult 是单个样本的回放结论。
type PolicyReplayResult struct {
	SampleID   int64              `json:"sample_id"`
	Expected   string             `json:"expected"`
	Actual     string             `json:"actual"`
	Score      *float64           `json:"score"`
	Categories []string           `json:"categories"`
	Nodes      []ReplayNodeResult `json:"nodes"`
	Error      string             `json:"error,omitempty"`
}

// ReplayNodeResult 是回放中一个节点的结论摘要。
type ReplayNodeResult struct {
	ModelID   string   `json:"model_id"`
	ModelName string   `json:"model_name"`
	Score     *float64 `json:"score"`
	Decision  string   `json:"decision"`
	Reason    string   `json:"reason"`
	Error     string   `json:"error,omitempty"`
}

// PolicyReplay 是一次政策回放的记录；只保存样本 ID 与结论，不保存正文。
type PolicyReplay struct {
	ID             int64                `json:"id"`
	Status         string               `json:"status"`
	ConfigSnapshot string               `json:"config_snapshot"`
	SampleIDs      []int64              `json:"sample_ids"`
	Results        []PolicyReplayResult `json:"results"`
	ErrorMessage   string               `json:"error_message"`
	CreatedBy      int64                `json:"created_by"`
	CreatedAt      time.Time            `json:"created_at"`
	FinishedAt     *time.Time           `json:"finished_at"`
}

// 回放状态：运行中、完成、失败、进程重启导致中断。
const (
	ReplayRunning     = "running"
	ReplayDone        = "done"
	ReplayFailed      = "failed"
	ReplayInterrupted = "interrupted"
)

// CreateReplay 创建回放记录；同一时间只允许一个回放在运行。
func (r *Repository) CreateReplay(ctx context.Context, config Config, sampleIDs []int64, actorID int64) (*PolicyReplay, error) {
	var running bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.policy_replays WHERE status='running')`).Scan(&running); err != nil {
		return nil, err
	}
	if running {
		return nil, fmt.Errorf("已有政策回放在运行，请等待完成")
	}
	snapshot, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	replay := &PolicyReplay{Status: ReplayRunning, ConfigSnapshot: string(snapshot), SampleIDs: sampleIDs, Results: []PolicyReplayResult{}}
	err = r.db.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.policy_replays(status,config_snapshot,sample_ids,results,created_by)
 VALUES('running',$1,$2,'[]',$3) RETURNING id,created_at`, string(snapshot), pq.Array(sampleIDs), actorID).Scan(&replay.ID, &replay.CreatedAt)
	if err != nil {
		return nil, err
	}
	replay.CreatedBy = actorID
	return replay, nil
}

// replayColumns 是回放记录共用的投影。
const replayColumns = `id,status,config_snapshot,sample_ids,results,COALESCE(error_message,''),created_by,created_at,finished_at`

func scanReplay(row rowScanner) (*PolicyReplay, error) {
	var replay PolicyReplay
	var results string
	if err := row.Scan(&replay.ID, &replay.Status, &replay.ConfigSnapshot, pq.Array(&replay.SampleIDs), &results, &replay.ErrorMessage,
		&replay.CreatedBy, &replay.CreatedAt, &replay.FinishedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(results), &replay.Results); err != nil {
		return nil, err
	}
	if replay.Results == nil {
		replay.Results = []PolicyReplayResult{}
	}
	return &replay, nil
}

// GetReplay 读取一次回放的进度与结果。
func (r *Repository) GetReplay(ctx context.Context, id int64) (*PolicyReplay, error) {
	replay, err := scanReplay(r.db.QueryRowContext(ctx, `SELECT `+replayColumns+` FROM sub2api_enhance.policy_replays WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return replay, err
}

// ListReplays 返回最近的回放记录。
func (r *Repository) ListReplays(ctx context.Context, limit int) ([]PolicyReplay, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+replayColumns+` FROM sub2api_enhance.policy_replays ORDER BY created_at DESC,id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]PolicyReplay, 0)
	for rows.Next() {
		replay, err := scanReplay(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *replay)
	}
	return items, rows.Err()
}

// SaveReplayResults 覆盖保存回放进度与状态。
func (r *Repository) SaveReplayResults(ctx context.Context, id int64, status string, results []PolicyReplayResult, errorMessage string) error {
	raw, err := json.Marshal(results)
	if err != nil {
		return err
	}
	finished := status != ReplayRunning
	var message any
	if errorMessage != "" {
		message = errorMessage
	}
	_, err = r.db.ExecContext(ctx, `UPDATE sub2api_enhance.policy_replays SET status=$2,results=$3,error_message=$4,
 finished_at=CASE WHEN $5 THEN clock_timestamp() END WHERE id=$1`, id, status, string(raw), message, finished)
	return err
}

// InterruptRunningReplays 在启动时把仍在运行的记录标记为中断，避免状态永久停留。
func (r *Repository) InterruptRunningReplays(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `UPDATE sub2api_enhance.policy_replays SET status='interrupted',finished_at=clock_timestamp(),error_message='服务重启，回放已中断'
 WHERE status='running'`)
	return err
}

// sampleIDsForReplay 返回回放选中的样本；为空表示全部样本。
func (r *Repository) sampleIDsForReplay(ctx context.Context, selected []int64) ([]int64, error) {
	if len(selected) > 0 {
		ids := append([]int64(nil), selected...)
		slices.Sort(ids)
		return ids, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM sub2api_enhance.policy_samples ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
