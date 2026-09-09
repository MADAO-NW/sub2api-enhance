package thirdpartypromptaudit

import (
	"slices"
	"time"
)

type IngressDecisionKind string

// 接入裁决只表示增强代理自身的决定，不冒充原版最终执行结果。
const (
	IngressDecisionAllow       IngressDecisionKind = "allow"
	IngressDecisionFlag        IngressDecisionKind = "flag"
	IngressDecisionBlock       IngressDecisionKind = "block"
	IngressDecisionUnavailable IngressDecisionKind = "unavailable"
)

type IntakeRequest struct {
	CapturedAt                                                                                                                     time.Time
	Manual                                                                                                                         bool
	CaptureKey, RequestID, ConversationKey, Username, UserEmail, APIKeyName, GroupName, Provider, Endpoint, Protocol, Model, Stage string
	UserID, APIKeyID                                                                                                               int64
	ActorUserID                                                                                                                    int64
	CaptureID                                                                                                                      *int64
	GroupID                                                                                                                        *int64
	Body                                                                                                                           []byte
	Background                                                                                                                     bool
}
type IntakeDecision struct {
	JobID     int64               `json:"job_id"`
	Mode      string              `json:"mode"`
	Kind      IngressDecisionKind `json:"kind"`
	ErrorCode string              `json:"error_code"`
}
type IngressDecision struct {
	Kind           IngressDecisionKind
	ErrorCode      string
	AllowNextStage bool
}

func (s *Service) Mode() string { return s.config.EffectiveMode() }

func (s *Service) ModeForUser(userID int64) string { return s.config.EffectiveModeForUser(userID) }

func (s *Service) CaptureWhenAuditOff() bool { return s.config.CaptureWhenAuditOff() }

// RequiresAudit 判断当前请求是否属于自动审核范围，供入口决定采集失败语义。
func (s *Service) RequiresAudit(userID int64, groupID *int64, provider string) bool {
	snapshot, err := s.config.Active()
	if err != nil {
		return s.config.EffectiveModeForUser(userID) != "off"
	}
	return snapshot.Mode != "off" && !slices.Contains(snapshot.ExcludedUserIDs, userID) &&
		(snapshot.AllGroups || (groupID != nil && slices.Contains(snapshot.GroupIDs, *groupID))) &&
		(len(snapshot.Platforms) == 0 || slices.Contains(snapshot.Platforms, provider))
}
