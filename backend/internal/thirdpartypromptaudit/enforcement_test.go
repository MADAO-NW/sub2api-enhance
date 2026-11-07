package thirdpartypromptaudit

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	infraerrors "sub2api-enhance/internal/pkg/errors"
)

type recordingEmailSender struct{ recipients []string }

func (sender *recordingEmailSender) SendEmail(_ context.Context, recipient, _, _ string) error {
	sender.recipients = append(sender.recipients, recipient)
	return nil
}

func TestIneligibleResultsCreateNoActionsAndAdministratorsCanReceiveWarnings(t *testing.T) {
	config := testConfig()
	config.Disable = DisableConfig{Enabled: true, Limit: 1}
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 1}
	state := EnforcementState{WarningArmed: true, DisableViolationCount: 1}
	next, actions := decideEnforcement(enforcementInput{State: state, User: EnforcementUser{Role: "user", Status: "active"},
		Eligible: false, TriggerBlock: true, Mode: "async", Config: config, Window: []Decision{DecisionBlock}})
	require.Empty(t, actions)
	require.Equal(t, state, next)
	next, actions = decideEnforcement(enforcementInput{State: state, User: EnforcementUser{Role: "admin", Status: "active"},
		Eligible: true, TriggerBlock: true, Mode: "async", Config: config, Window: []Decision{DecisionBlock}})
	require.Equal(t, []string{"warning"}, actions)
	require.False(t, next.WarningArmed)
}

func TestOneViolationCanCreateWarningAndDisableActions(t *testing.T) {
	config := testConfig()
	config.Disable = DisableConfig{Enabled: true, Limit: 1}
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 1}
	state := EnforcementState{WarningArmed: true, DisableViolationCount: 1}
	next, actions := decideEnforcement(enforcementInput{State: state, User: EnforcementUser{Role: "user", Status: "active"},
		Eligible: true, TriggerBlock: true, Mode: "async", Config: config, Window: []Decision{DecisionBlock}})
	require.Equal(t, []string{"warning", "disable"}, actions)
	require.False(t, next.WarningArmed)
}

func TestWarningRearmsOnlyAfterWindowFallsBelowLimit(t *testing.T) {
	config := testConfig()
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 2}
	state := EnforcementState{WarningArmed: true}
	user := EnforcementUser{Role: "user", Status: "active"}
	decide := func(state EnforcementState, window []Decision, trigger bool) (EnforcementState, []string) {
		return decideEnforcement(enforcementInput{State: state, User: user, Eligible: true, TriggerBlock: trigger,
			Mode: "async", Config: config, Window: window})
	}
	state, actions := decide(state, []Decision{DecisionBlock, DecisionBlock}, true)
	require.Equal(t, []string{"warning"}, actions)
	state, actions = decide(state, []Decision{DecisionBlock, DecisionBlock, DecisionBlock}, true)
	require.Empty(t, actions)
	state, actions = decide(state, []Decision{DecisionPass, DecisionPass, DecisionBlock}, false)
	require.Empty(t, actions)
	require.True(t, state.WarningArmed)
	_, actions = decide(state, []Decision{DecisionBlock, DecisionPass, DecisionBlock}, true)
	require.Equal(t, []string{"warning"}, actions)
}

// 自动加严只在提醒触发、用户原本是异步审核且尚未加严时产生。
func TestEscalationRequiresAsyncWarningAndIsNotRepeated(t *testing.T) {
	config := testConfig()
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 1}
	config.EscalateOnWarning = true
	user := EnforcementUser{Role: "user", Status: "active"}
	state := EnforcementState{WarningArmed: true}
	input := enforcementInput{State: state, User: user, Eligible: true, TriggerBlock: true,
		Mode: "async", Config: config, Window: []Decision{DecisionBlock}}
	_, actions := decideEnforcement(input)
	require.Equal(t, []string{"warning", "escalation"}, actions)
	escalatedAt := time.Now()
	state.EscalatedAt = &escalatedAt
	state.WarningArmed = true
	input.State = state
	_, actions = decideEnforcement(input)
	require.Equal(t, []string{"warning"}, actions)
	state.EscalatedAt = nil
	input.State = state
	input.Mode = "blocking"
	_, actions = decideEnforcement(input)
	require.Equal(t, []string{"warning"}, actions)
}

// 类别处置：立即停用不必等累计阈值，仅通知不产生停用，且都只对普通用户生效。
func TestCategoryDispositionChangesDisableDecision(t *testing.T) {
	config := testConfig()
	config.Disable = DisableConfig{Enabled: true, Limit: 5}
	user := EnforcementUser{Role: "user", Status: "active"}
	state := EnforcementState{DisableViolationCount: 1}
	_, actions := decideEnforcement(enforcementInput{State: state, User: user, Eligible: true, TriggerBlock: true,
		Mode: "blocking", Config: config, Disposition: CategoryDispositionImmediateDisable})
	require.Equal(t, []string{"disable"}, actions)
	_, actions = decideEnforcement(enforcementInput{State: state, User: user, Eligible: true, TriggerBlock: true,
		Mode: "blocking", Config: config, Disposition: CategoryDispositionNoticeOnly})
	require.Empty(t, actions)
	_, actions = decideEnforcement(enforcementInput{State: EnforcementState{DisableViolationCount: 9}, User: EnforcementUser{Role: "admin", Status: "active"},
		Eligible: true, TriggerBlock: true, Mode: "blocking", Config: config, Disposition: CategoryDispositionImmediateDisable})
	require.Empty(t, actions)
}

func TestBlockingNoticeRecipientsAreIndependentAndDeduplicated(t *testing.T) {
	outcomeID := int64(9)
	action := Action{ActionType: "blocking_notice", JobID: &outcomeID}
	notice := noticeContext{User: EnforcementUser{Username: "user", Email: "user@example.invalid"}, AdminEmail: "admin@example.invalid",
		PublicOrigin: "https://enhance.example.invalid", JobID: outcomeID, RequestID: "enh-1", TriggerKind: PromptKindUser, Now: time.Now()}
	deliveries := jobActionDeliveries(action, notice)
	require.Len(t, deliveries, 2)
	require.Equal(t, "admin", deliveries[0].Kind)
	require.Equal(t, "user", deliveries[1].Kind)
	require.Contains(t, deliveries[0].Subject, "请求已阻止")
	require.Contains(t, deliveries[0].Body, "https://enhance.example.invalid/enhance/third-party-prompt-audit?tab=jobs&amp;job_id=9")
	require.Contains(t, deliveries[0].Body, "请求 ID：enh-1")
	require.Contains(t, deliveries[0].Body, "当前回合用户提示词")
	require.Contains(t, deliveries[1].Body, "参考编号：#9")
	require.NotContains(t, deliveries[1].Body, "管理后台")
	require.NotContains(t, deliveries[1].Body, "enhance.example.invalid")
	notice.AdminEmail = "user@example.invalid"
	require.Len(t, jobActionDeliveries(action, notice), 1)
	notice.AdminEmail = ""
	require.Len(t, jobActionDeliveries(action, notice), 1)
}

// 用户正文按动作类型生成，不含哈希与模型理由；加严说明在合并时并入提醒邮件。
func TestUserNoticeBodyAvoidsInternalDetails(t *testing.T) {
	notice := noticeContext{JobID: 12, RequestID: "enh-2", Categories: []string{"R07"},
		CategoryNames: map[string]string{"R07": "恶意网络活动与关键基础设施"}, Now: time.Now(),
		Window: []Decision{DecisionBlock, DecisionPass}, Disable: DisableConfig{Enabled: true, Limit: 3}}
	blocked := notice.userNoticeBody("blocking_notice")
	require.Contains(t, blocked, "参考编号：#12")
	require.Contains(t, blocked, "R07 恶意网络活动与关键基础设施")
	require.Contains(t, blocked, "如认为是误判")
	warning := notice.userNoticeBody("warning")
	require.Contains(t, warning, "最近 2 次请求中有 1 次未通过内容审核")
	require.Contains(t, warning, "累计 3 次违规将停用账号")
	require.NotContains(t, warning, "#12")
	notice.MergedEscalation = true
	require.Contains(t, notice.userNoticeBody("warning"), "响应可能变慢")
	require.Contains(t, notice.userNoticeBody("disable"), "账号因累计违规已被停用")
	require.Contains(t, notice.userNoticeBody("escalation"), "参考编号：#12")
}

func TestNotificationWorkerSendsAdminAndUserDeliveriesIndependently(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	for range 5 {
		mock.ExpectExec("UPDATE sub2api_enhance.third_party_prompt_audit_enforcement_actions SET notification_status").WillReturnResult(sqlmock.NewResult(0, 1))
	}
	sender := &recordingEmailSender{}
	service := &Service{repo: NewRepository(db), email: sender, metrics: NewRuntimeMetrics()}
	outcomeID := int64(9)
	leaseUntil := time.Now().Add(time.Minute)
	action := &Action{ID: 3, UserID: 7, JobID: &outcomeID, ActionType: "warning", ExecutionStatus: "succeeded", NotificationStatus: "pending", BusinessSnapshot: map[string]any{}, RuleSnapshot: map[string]any{}, ClaimGeneration: 1, LeaseUntil: &leaseUntil, Deliveries: []Delivery{
		{Recipient: "admin@example.invalid", Kind: "admin", Status: "pending", Attempts: []DeliveryAttempt{}},
		{Recipient: "user@example.invalid", Kind: "user", Status: "pending", Attempts: []DeliveryAttempt{}},
	}}
	service.processAction(context.Background(), action)
	require.Equal(t, []string{"admin@example.invalid", "user@example.invalid"}, sender.recipients)
	require.Equal(t, "sent", action.NotificationStatus)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTestAdminEmailUsesAppliedAdministratorRecipient(t *testing.T) {
	config := testConfig()
	config.AdminEmail = "admin@example.invalid"
	manager := &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: config}}}
	sender := &recordingEmailSender{}
	service := &Service{config: manager, email: sender}

	require.NoError(t, service.TestAdminEmail(context.Background()))
	require.Equal(t, []string{"admin@example.invalid"}, sender.recipients)
}

func TestTestAdminEmailRejectsMissingAdministratorRecipient(t *testing.T) {
	manager := &ConfigManager{active: &activeConfig{Stored: storedConfig{Config: testConfig()}}}
	service := &Service{config: manager, email: &recordingEmailSender{}}

	err := service.TestAdminEmail(context.Background())
	require.Equal(t, 400, infraerrors.Code(err))
	require.Equal(t, "third_party_audit_admin_email_missing", infraerrors.Reason(err))
}
