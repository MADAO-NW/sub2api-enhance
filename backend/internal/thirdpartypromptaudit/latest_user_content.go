package thirdpartypromptaudit

import (
	"context"
	"strings"
)

type LatestUserContent struct {
	Content           *string                  `json:"content"`
	Items             []CurrentUserContentItem `json:"items"`
	CombinedCount     int                      `json:"combined_count"`
	UnavailableReason string                   `json:"unavailable_reason"`
}

type CurrentUserContentItem struct {
	Order      int    `json:"order"`
	SourcePath string `json:"source_path"`
	Content    string `json:"content"`
}

// latestUserContent 返回本次审核使用的全部当前任务 user 文本，保留旧接口名称。
func latestUserContent(snapshot *InputSnapshot) LatestUserContent {
	segments, err := ExtractSegments(snapshot, "full_request")
	if err != nil {
		return LatestUserContent{Items: []CurrentUserContentItem{}, UnavailableReason: "input_unavailable"}
	}
	result := LatestUserContent{Items: []CurrentUserContentItem{}}
	combined := make([]string, 0)
	for _, segment := range segments {
		if segment.SourceRole != "user" || segment.TurnScope != "current" {
			continue
		}
		parts := make([]string, 0, len(segment.Content))
		for _, block := range segment.Content {
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) > 0 {
			content := strings.Join(parts, "\n")
			result.Items = append(result.Items, CurrentUserContentItem{Order: segment.Order, SourcePath: segment.SourcePath, Content: content})
			combined = append(combined, content)
		}
	}
	if len(combined) == 0 {
		result.UnavailableReason = "current_user_not_found"
		return result
	}
	content := strings.Join(combined, "\n\n")
	result.Content, result.CombinedCount = &content, len(result.Items)
	return result
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
		return LatestUserContent{Items: []CurrentUserContentItem{}, UnavailableReason: "input_unavailable"}
	}
	snapshot, err := CaptureInput(protocol, body)
	if err != nil {
		return LatestUserContent{Items: []CurrentUserContentItem{}, UnavailableReason: "input_unavailable"}
	}
	return latestUserContent(snapshot)
}
