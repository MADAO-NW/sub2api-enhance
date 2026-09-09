package thirdpartypromptaudit

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sub2api-enhance/internal/sub2api"
	"sync/atomic"
	"time"
)

type Capture struct {
	ID               int64             `json:"id"`
	Key              string            `json:"capture_key"`
	ConversationKey  string            `json:"conversation_key,omitempty"`
	Transport        string            `json:"transport"`
	ConnectionKey    *string           `json:"connection_key"`
	Sequence         *int64            `json:"message_sequence"`
	Protocol         string            `json:"protocol"`
	Format           string            `json:"body_format"`
	Raw              []byte            `json:"raw_body,omitempty"`
	SHA256           string            `json:"body_sha256"`
	Bytes            int64             `json:"body_bytes"`
	SnapshotStatus   string            `json:"snapshot_status"`
	Metadata         map[string]string `json:"metadata"`
	Identity         sub2api.Identity  `json:"identity"`
	Eligibility      string            `json:"eligibility_status"`
	ProcessingStatus string            `json:"processing_status"`
	ForwardingStatus string            `json:"forwarding_status"`
	Observations     []map[string]any  `json:"forwarding_observations"`
	Error            string            `json:"last_error_message"`
	CreatedAt        time.Time         `json:"created_at"`
}
type CaptureStore struct {
	db       *sql.DB
	failures atomic.Uint64
}

func NewCaptureStore(db *sql.DB) *CaptureStore { return &CaptureStore{db: db} }
func (s *CaptureStore) Save(ctx context.Context, c *Capture) error {
	if c.Raw == nil {
		c.Raw = []byte{}
	}
	hash := sha256.Sum256(c.Raw)
	c.SHA256 = hex.EncodeToString(hash[:])
	c.Bytes = int64(len(c.Raw))
	metadata, err := json.Marshal(c.Metadata)
	if err != nil {
		return err
	}
	identity, err := json.Marshal(c.Identity)
	if err != nil {
		return err
	}
	var userID, keyID any
	var resolved any
	if c.Identity.UserID > 0 {
		userID = c.Identity.UserID
		keyID = c.Identity.APIKeyID
		resolved = c.Identity.ResolvedAt
	}
	c.Eligibility = c.Identity.Eligibility
	if c.Eligibility == "" {
		c.Eligibility = "unknown"
	}
	c.ProcessingStatus = "queued"
	if c.Eligibility != "passed" || c.SnapshotStatus != "complete" {
		c.ProcessingStatus = "skipped"
	}
	c.ForwardingStatus = "not_forwarded"
	err = s.db.QueryRowContext(ctx, `INSERT INTO sub2api_enhance.captures(capture_key,conversation_key,transport,connection_key,message_sequence,protocol,body_format,raw_body,body_sha256,body_bytes,snapshot_status,request_metadata,identity_snapshot,identity_resolved_at,user_id,api_key_id,group_id,eligibility_status,processing_status,last_error_message)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20) RETURNING id,created_at`, c.Key, nullIfEmpty(c.ConversationKey), c.Transport, c.ConnectionKey, c.Sequence, c.Protocol, c.Format, c.Raw, c.SHA256, c.Bytes, c.SnapshotStatus, string(metadata), string(identity), resolved, userID, keyID, c.Identity.GroupID, c.Eligibility, c.ProcessingStatus, encodeStoredText(c.Error)).Scan(&c.ID, &c.CreatedAt)
	if err != nil {
		// 提交不确定时按本次服务端键回查，只确认原文，不重放生成请求。
		verify, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
		defer cancel()
		var digest string
		if readErr := s.db.QueryRowContext(verify, `SELECT id,created_at,body_sha256 FROM sub2api_enhance.captures WHERE capture_key=$1`, c.Key).Scan(&c.ID, &c.CreatedAt, &digest); readErr == nil && digest == c.SHA256 {
			return nil
		}
	}
	if err != nil {
		s.failures.Add(1)
	}
	return err
}
func (s *CaptureStore) Observe(ctx context.Context, id int64, status string, detail map[string]any) error {
	detail["at"] = time.Now().UTC()
	detail["status"] = status
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE sub2api_enhance.captures SET forwarding_status=$2,forwarding_observations=(forwarding_observations::jsonb || jsonb_build_array($3::jsonb))::text,updated_at=clock_timestamp() WHERE id=$1`, id, status, string(raw))
	return err
}
func (s *CaptureStore) Finish(ctx context.Context, id int64, status, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sub2api_enhance.captures SET processing_status=$2,last_error_message=$3,next_attempt_at=clock_timestamp()+interval '5 seconds',updated_at=clock_timestamp() WHERE id=$1 AND processing_status<>'done'`, id, status, encodeStoredText(message))
	return err
}
func (s *CaptureStore) Get(ctx context.Context, id int64) (*Capture, error) {
	var c Capture
	var meta, identity, obs string
	var message *string
	err := s.db.QueryRowContext(ctx, `SELECT id,capture_key,COALESCE(conversation_key,''),transport,connection_key,message_sequence,protocol,body_format,raw_body,body_sha256,body_bytes,snapshot_status,request_metadata,COALESCE(identity_snapshot,'{}'),eligibility_status,processing_status,forwarding_status,forwarding_observations,last_error_message,created_at FROM sub2api_enhance.captures WHERE id=$1`, id).Scan(&c.ID, &c.Key, &c.ConversationKey, &c.Transport, &c.ConnectionKey, &c.Sequence, &c.Protocol, &c.Format, &c.Raw, &c.SHA256, &c.Bytes, &c.SnapshotStatus, &meta, &identity, &c.Eligibility, &c.ProcessingStatus, &c.ForwardingStatus, &obs, &message, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	for _, v := range []struct {
		raw    string
		target any
	}{{meta, &c.Metadata}, {identity, &c.Identity}, {obs, &c.Observations}} {
		if err := json.Unmarshal([]byte(v.raw), v.target); err != nil {
			return nil, err
		}
	}
	if message != nil {
		c.Error = decodeStoredText(*message)
	}
	return &c, nil
}
func (s *CaptureStore) List(ctx context.Context, page, size int, status string) (Page[Capture], error) {
	out := Page[Capture]{Page: page, PageSize: size, Items: []Capture{}}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.captures WHERE ($1='' OR processing_status=$1)`, status).Scan(&out.Total); err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,capture_key,COALESCE(conversation_key,''),transport,protocol,body_format,body_sha256,body_bytes,snapshot_status,eligibility_status,processing_status,forwarding_status,last_error_message,created_at FROM sub2api_enhance.captures WHERE ($1='' OR processing_status=$1) ORDER BY id DESC LIMIT $2 OFFSET $3`, status, size, (page-1)*size)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Capture
		var message *string
		if err := rows.Scan(&c.ID, &c.Key, &c.ConversationKey, &c.Transport, &c.Protocol, &c.Format, &c.SHA256, &c.Bytes, &c.SnapshotStatus, &c.Eligibility, &c.ProcessingStatus, &c.ForwardingStatus, &message, &c.CreatedAt); err != nil {
			return out, err
		}
		if message != nil {
			c.Error = decodeStoredText(*message)
		}
		out.Items = append(out.Items, c)
	}
	return out, rows.Err()
}

// AuditCapture 从可靠原文派生审核输入；解析失败保留原文，不生成猜测结论。
func (s *Service) AuditCapture(ctx context.Context, c *Capture) (*IntakeDecision, error) {
	request, err := captureRequest(c)
	if err != nil {
		return nil, err
	}
	return s.Check(ctx, request), nil
}

// captureRequest 供正式采集和人工复核从同一原文按当前提取规则生成输入。
func captureRequest(c *Capture) (IntakeRequest, error) {
	if c.SnapshotStatus != "complete" || c.Identity.UserID <= 0 {
		return IntakeRequest{}, errors.New("采集输入或身份不完整")
	}
	raw := c.Raw
	if encoding := c.Metadata["content_encoding"]; encoding != "" && encoding != "identity" {
		var reader io.ReadCloser
		var err error
		switch encoding {
		case "gzip":
			reader, err = gzip.NewReader(bytes.NewReader(raw))
		case "deflate":
			reader = flate.NewReader(bytes.NewReader(raw))
		default:
			err = errors.New("该内容编码尚未支持审核解析")
		}
		if err != nil {
			return IntakeRequest{}, err
		}
		raw, err = io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			return IntakeRequest{}, err
		}
	}
	request := IntakeRequest{CapturedAt: c.CreatedAt, CaptureKey: c.Key, CaptureID: &c.ID, RequestID: c.Key, ConversationKey: c.ConversationKey, UserID: c.Identity.UserID, APIKeyID: c.Identity.APIKeyID, GroupID: c.Identity.GroupID, Username: c.Identity.Username, UserEmail: c.Identity.UserEmail, APIKeyName: c.Identity.APIKeyName, GroupName: c.Identity.GroupName, Provider: c.Identity.Platform, Endpoint: c.Metadata["path"], Protocol: c.Protocol, Stage: "external_ingress", Body: raw, Background: c.Metadata["background"] == "true", Manual: c.Metadata["manual_reprocess"] == "true"}
	if c.Format == "multipart_text_fields" {
		var fields []map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			return IntakeRequest{}, err
		}
		texts := []any{}
		for _, field := range fields {
			if text, ok := field["text"].(string); ok {
				texts = append(texts, map[string]any{"type": "text", "text": text, "source_name": field["name"], "source_order": field["order"]})
			}
		}
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": texts}}}
		raw, err := json.Marshal(body)
		if err != nil {
			return IntakeRequest{}, err
		}
		request.Body = raw
		request.Protocol = "chat_completions"
	}
	var model struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(request.Body, &model) == nil {
		request.Model = model.Model
	}
	return request, nil
}

// captureLoop 仅恢复落库后的异步派生任务；正式 Job 的唯一采集引用防止重复创建。
func (s *Service) captureLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	store := NewCaptureStore(s.repo.db)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if s.Mode() == "off" {
			continue
		}
		for ctx.Err() == nil {
			var id, generation int64
			err := s.repo.db.QueryRowContext(ctx, `WITH candidate AS (
    SELECT id FROM sub2api_enhance.captures WHERE (processing_status IN ('queued','retry') OR (processing_status='processing' AND lease_until<clock_timestamp()))
    AND next_attempt_at<=clock_timestamp() AND created_at<clock_timestamp()-interval '10 seconds' AND eligibility_status='passed' AND request_metadata::json->>'mode'='async'
    ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
    UPDATE sub2api_enhance.captures SET processing_status='processing',claim_generation=claim_generation+1,lease_until=clock_timestamp()+interval '30 seconds',processing_attempts=processing_attempts+1
    WHERE id IN(SELECT id FROM candidate) RETURNING id,claim_generation`).Scan(&id, &generation)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				s.noteError("capture_recovery_query_failed", err)
				break
			}
			capture, err := store.Get(ctx, id)
			if err != nil {
				s.noteError("capture_recovery_read_failed", err)
				break
			}
			capture.Metadata["background"] = "true"
			result, err := s.AuditCapture(ctx, capture)
			state, message := "done", ""
			if err != nil {
				state = "failed"
				message = err.Error()
			} else if result == nil || result.JobID == 0 {
				state = "retry"
			}
			update, err := s.repo.db.ExecContext(ctx, `UPDATE sub2api_enhance.captures SET processing_status=$3,last_error_message=$4,lease_until=NULL,next_attempt_at=clock_timestamp()+interval '5 seconds',updated_at=clock_timestamp() WHERE id=$1 AND claim_generation=$2 AND lease_until>clock_timestamp()`, id, generation, state, encodeStoredText(message))
			if err := checkLeaseUpdate(update, err); err != nil {
				s.noteError("capture_recovery_persist_failed", err)
			}
		}
	}
}
