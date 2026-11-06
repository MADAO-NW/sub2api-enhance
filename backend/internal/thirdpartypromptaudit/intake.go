package thirdpartypromptaudit

// IngressDecisionKind 只表示增强代理自身的决定，不冒充原版最终执行结果。
type IngressDecisionKind string

const (
	IngressDecisionAllow       IngressDecisionKind = "allow"
	IngressDecisionFlag        IngressDecisionKind = "flag"
	IngressDecisionBlock       IngressDecisionKind = "block"
	IngressDecisionUnavailable IngressDecisionKind = "unavailable"
)

// IntakeRequest 是入口已经完成协议解析后交给审核服务的请求；不携带请求原文。
type IntakeRequest struct {
	Parsed      *ParsedRequest
	Identity    Identity
	GroupID     *int64
	UserID      int64
	APIKeyID    int64
	ActorUserID int64
	Provider    string
	Model       string
	RequestID   string
	Background  bool
}

type IntakeDecision struct {
	JobID     int64               `json:"job_id"`
	Mode      string              `json:"mode"`
	Kind      IngressDecisionKind `json:"kind"`
	ErrorCode string              `json:"error_code"`
	// TurnKey 供入口在收到上游响应后登记 previous_response_id 映射。
	TurnKey string `json:"-"`
}

// Mode 返回全局审核模式，供入口在全局关闭时跳过解析与审核。
func (s *Service) Mode() string { return s.config.EffectiveMode() }

// ModeForUser 返回该用户生效的审核模式，供入口决定身份或协议不可用时的语义。
func (s *Service) ModeForUser(userID int64) string { return s.config.EffectiveModeForUser(userID) }
