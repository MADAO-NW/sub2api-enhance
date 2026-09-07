package thirdpartypromptaudit

import (
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
