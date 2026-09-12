package thirdpartypromptaudit

import (
	"context"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	infraerrors "sub2api-enhance/internal/pkg/errors"
	"testing"
	"time"
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
	next, actions := decideEnforcement(state, EnforcementUser{Role: "user", Status: "active"}, false, true, config, []Decision{DecisionBlock})
	require.Empty(t, actions)
	require.Equal(t, state, next)
	next, actions = decideEnforcement(state, EnforcementUser{Role: "admin", Status: "active"}, true, true, config, []Decision{DecisionBlock})
	require.Equal(t, []string{"warning"}, actions)
	require.False(t, next.WarningArmed)
}

func TestOneViolationCanCreateWarningAndDisableActions(t *testing.T) {
	config := testConfig()
	config.Disable = DisableConfig{Enabled: true, Limit: 1}
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 1}
	state := EnforcementState{WarningArmed: true, DisableViolationCount: 1}
	next, actions := decideEnforcement(state, EnforcementUser{Role: "user", Status: "active"}, true, true, config, []Decision{DecisionBlock})
	require.Equal(t, []string{"warning", "disable"}, actions)
	require.False(t, next.WarningArmed)
}

func TestWarningRearmsOnlyAfterWindowFallsBelowLimit(t *testing.T) {
	config := testConfig()
	config.Warning = WarningConfig{Enabled: true, Window: 3, Limit: 2}
	state := EnforcementState{WarningArmed: true}
	user := EnforcementUser{Role: "user", Status: "active"}
	state, actions := decideEnforcement(state, user, true, true, config, []Decision{DecisionBlock, DecisionBlock})
	require.Equal(t, []string{"warning"}, actions)
	state, actions = decideEnforcement(state, user, true, true, config, []Decision{DecisionBlock, DecisionBlock, DecisionBlock})
	require.Empty(t, actions)
	state, actions = decideEnforcement(state, user, true, false, config, []Decision{DecisionPass, DecisionPass, DecisionBlock})
	require.Empty(t, actions)
	require.True(t, state.WarningArmed)
	_, actions = decideEnforcement(state, user, true, true, config, []Decision{DecisionBlock, DecisionPass, DecisionBlock})
	require.Equal(t, []string{"warning"}, actions)
}

func TestBlockingNoticeRecipientsAreIndependentAndDeduplicated(t *testing.T) {
	outcomeID := int64(9)
	action := Action{ActionType: "blocking_notice", OutcomeID: &outcomeID}
	user := EnforcementUser{Username: "user", Email: "user@example.invalid"}
	deliveries := actionDeliveries(action, user, "admin@example.invalid")
	require.Len(t, deliveries, 2)
	require.Equal(t, "admin", deliveries[0].Kind)
	require.Equal(t, "user", deliveries[1].Kind)
	require.Contains(t, deliveries[0].Subject, "请求已阻止")
	require.Len(t, actionDeliveries(action, user, "user@example.invalid"), 1)
	require.Len(t, actionDeliveries(action, user, ""), 1)
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
	action := &Action{ID: 3, UserID: 7, OutcomeID: &outcomeID, ActionType: "warning", ExecutionStatus: "succeeded", NotificationStatus: "pending", BusinessSnapshot: map[string]any{}, RuleSnapshot: map[string]any{}, ClaimGeneration: 1, LeaseUntil: &leaseUntil, Deliveries: []Delivery{
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

func TestDisableContributionFollowsTheLatestTaskDecision(t *testing.T) {
	count, counted := adjustDisableContribution(4, false, true)
	require.EqualValues(t, 5, count)
	require.True(t, counted)
	count, counted = adjustDisableContribution(count, counted, true)
	require.EqualValues(t, 5, count)
	count, counted = adjustDisableContribution(count, counted, false)
	require.EqualValues(t, 4, count)
	require.False(t, counted)
	count, counted = adjustDisableContribution(0, true, false)
	require.Zero(t, count)
	require.False(t, counted)
}
