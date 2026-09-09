package thirdpartypromptaudit

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"sub2api-enhance/internal/sub2api"
	"testing"
	"time"
)

func TestUncertainCaptureCommitRequiresDigestConfirmation(t *testing.T) {
	for _, match := range []bool{false, true} {
		t.Run(map[bool]string{false: "无法确认提交", true: "相同原文已提交"}[match], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			store := NewCaptureStore(db)
			raw := []byte(" {\"input\":\"a\\u0000b\",\"id\":9007199254740993} ")
			sum := sha256.Sum256(raw)
			digest := hex.EncodeToString(sum[:])
			if !match {
				digest = "different"
			}
			mock.ExpectQuery("INSERT INTO sub2api_enhance.captures").WillReturnError(errors.New("commit response lost"))
			mock.ExpectQuery("SELECT id,created_at,body_sha256").WithArgs("capture-1").WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "body_sha256"}).AddRow(7, time.Now(), digest))
			capture := &Capture{Key: "capture-1", Transport: "http", Protocol: "responses", Format: "entity_bytes", Raw: raw, SnapshotStatus: "complete", Metadata: map[string]string{}, Identity: sub2api.Identity{Eligibility: "unknown"}}
			err = store.Save(context.Background(), capture)
			if match {
				require.NoError(t, err)
				require.Zero(t, store.failures.Load())
			} else {
				require.Error(t, err)
				require.EqualValues(t, 1, store.failures.Load())
			}
			require.Equal(t, raw, capture.Raw)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestCaptureListIncludesUsernameAndEmailForDisplay(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM sub2api_enhance.captures").WithArgs("").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	columns := []string{"id", "capture_key", "conversation_key", "transport", "protocol", "body_format", "body_sha256", "body_bytes", "snapshot_status", "eligibility_status", "processing_status", "forwarding_status", "last_error_message", "created_at", "display_username", "display_email", "user_id"}
	mock.ExpectQuery("SELECT c.id,c.capture_key").WithArgs("", 20, 0).WillReturnRows(sqlmock.NewRows(columns).AddRow(3, "capture-3", "", "http", "responses", "entity_bytes", "sha", 12, "complete", "passed", "done", "complete", nil, time.Now(), "测试用户", "user@example.invalid", 7))
	page, err := NewCaptureStore(db).List(context.Background(), 1, 20, "")
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, "测试用户", page.Items[0].DisplayUsername)
	require.Equal(t, "user@example.invalid", page.Items[0].DisplayEmail)
	require.EqualValues(t, 7, page.Items[0].Identity.UserID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMultipartRepeatedTextFieldsRemainSeparate(t *testing.T) {
	raw, _ := json.Marshal([]map[string]any{{"name": "prompt", "order": 1, "text": "first"}, {"name": "prompt", "order": 2, "text": "second"}, {"name": "image", "order": 3, "binary_omitted": true}})
	request, err := captureRequest(&Capture{Raw: raw, Format: "multipart_text_fields", SnapshotStatus: "complete", Identity: sub2api.Identity{UserID: 1}, Metadata: map[string]string{"manual_reprocess": "true", "background": "true"}})
	require.NoError(t, err)
	require.True(t, request.Manual)
	require.True(t, request.Background)
	snapshot, err := CaptureInput(request.Protocol, request.Body)
	require.NoError(t, err)
	text, _ := json.Marshal(snapshot)
	require.Contains(t, string(text), "first")
	require.Contains(t, string(text), "second")
}

func TestCaptureLatestUserContentDoesNotRequireResolvedIdentity(t *testing.T) {
	capture := &Capture{Protocol: "responses", Format: "entity_bytes", Raw: []byte(`{"input":"latest capture text"}`), SnapshotStatus: "complete", Metadata: map[string]string{}}
	result := captureLatestUserContent(capture)
	require.NotNil(t, result.Content)
	require.Equal(t, "latest capture text", *result.Content)
}

func TestCaptureLatestUserContentRestoresCompressedAndMultipartBodies(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, err := writer.Write([]byte(`{"messages":[{"role":"user","content":"compressed"}]}`))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	result := captureLatestUserContent(&Capture{Protocol: "openai_chat", Format: "entity_bytes", Raw: compressed.Bytes(), SnapshotStatus: "complete", Metadata: map[string]string{"content_encoding": "gzip"}})
	require.NotNil(t, result.Content)
	require.Equal(t, "compressed", *result.Content)

	raw, err := json.Marshal([]map[string]any{{"name": "prompt", "order": 1, "text": "first"}, {"name": "prompt", "order": 2, "text": "second"}})
	require.NoError(t, err)
	result = captureLatestUserContent(&Capture{Format: "multipart_text_fields", Raw: raw, SnapshotStatus: "complete", Metadata: map[string]string{}})
	require.NotNil(t, result.Content)
	require.Equal(t, "first\nsecond", *result.Content)
}

func TestCaptureLatestUserContentReportsUnavailableInput(t *testing.T) {
	for _, capture := range []*Capture{
		{Protocol: "unknown", Format: "entity_bytes", Raw: []byte(`{}`), SnapshotStatus: "complete", Metadata: map[string]string{}},
		{Protocol: "openai_chat", Format: "entity_bytes", SnapshotStatus: "incomplete", Metadata: map[string]string{}},
	} {
		result := captureLatestUserContent(capture)
		require.Nil(t, result.Content)
		require.Equal(t, "input_unavailable", result.UnavailableReason)
	}
}
