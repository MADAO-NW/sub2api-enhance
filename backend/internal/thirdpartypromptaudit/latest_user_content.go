package thirdpartypromptaudit

import (
	"context"
	"strings"
)

type LatestUserContent struct {
	Content           *string `json:"content"`
	UnavailableReason string  `json:"unavailable_reason"`
}

// latestUserContent 从完整输入中读取最后一个真实 user 片段，不回退到其他角色。
func latestUserContent(snapshot *InputSnapshot) LatestUserContent {
	segments, err := ExtractSegments(snapshot, "full_request")
	if err != nil {
		return LatestUserContent{UnavailableReason: "input_unavailable"}
	}
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i].SourceRole != "user" {
			continue
		}
		parts := make([]string, 0, len(segments[i].Content))
		for _, block := range segments[i].Content {
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) > 0 {
			content := strings.Join(parts, "\n")
			return LatestUserContent{Content: &content}
		}
	}
	return LatestUserContent{UnavailableReason: "user_content_not_found"}
}

func (r *Repository) JobLatestUserContent(ctx context.Context, id int64) (LatestUserContent, error) {
	job, err := r.GetJob(ctx, id, true)
	if err != nil {
		return LatestUserContent{}, err
	}
	return latestUserContent(job.FullInput), nil
}

func captureLatestUserContent(capture *Capture) LatestUserContent {
	protocol, body, err := captureAuditBody(capture)
	if err != nil {
		return LatestUserContent{UnavailableReason: "input_unavailable"}
	}
	snapshot, err := CaptureInput(protocol, body)
	if err != nil {
		return LatestUserContent{UnavailableReason: "input_unavailable"}
	}
	return latestUserContent(snapshot)
}
