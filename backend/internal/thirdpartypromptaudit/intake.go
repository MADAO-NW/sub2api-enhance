package thirdpartypromptaudit

import "time"

type IngressDecisionKind string

// 接入裁决只表示增强代理自身的决定，不冒充原版最终执行结果。
const (
	IngressDecisionAllow       IngressDecisionKind = "allow"
	IngressDecisionFlag        IngressDecisionKind = "flag"
	IngressDecisionBlock       IngressDecisionKind = "block"
	IngressDecisionUnavailable IngressDecisionKind = "unavailable"
)

type IntakeRequest struct {
	CapturedAt                                                                                                    time.Time
	Manual                                                                                                        bool
	CaptureKey, RequestID, Username, UserEmail, APIKeyName, GroupName, Provider, Endpoint, Protocol, Model, Stage string
	UserID, APIKeyID                                                                                              int64
	CaptureID                                                                                                     *int64
	GroupID                                                                                                       *int64
	Body                                                                                                          []byte
	Background                                                                                                    bool
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
