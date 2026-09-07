package thirdpartypromptaudit

import (
	"context"
	"errors"
	"net/mail"
	"net/textproto"
	"sync"
	"time"

	"sub2api-enhance/internal/notify"
	"sub2api-enhance/internal/pkg/logger"
)

func (s *Service) run() {
	defer s.wg.Done()
	defer s.running.Store(false)
	var tasks sync.WaitGroup
	tasks.Add(2)
	go func() { defer tasks.Done(); s.actionLoop(s.ctx) }()
	go func() { defer tasks.Done(); s.captureLoop(s.ctx) }()
	defer tasks.Wait()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastReload := time.Now()
	for {
		if s.ctx.Err() != nil {
			return
		}
		if time.Since(lastReload) >= 5*time.Second {
			readCtx, cancel := context.WithTimeout(s.ctx, persistenceTimeout)
			err := s.config.Reload(readCtx)
			cancel()
			if err != nil && s.ctx.Err() == nil {
				s.noteError("config_load_failed", err)
			}
			recoverCtx, recoverCancel := context.WithTimeout(s.ctx, persistenceTimeout)
			if err := s.repo.RecoverExpired(recoverCtx); err != nil && s.ctx.Err() == nil {
				s.noteError("lease_recovery_failed", err)
			}
			recoverCancel()
			lastReload = time.Now()
		}
		// Worker 数是评估并发槽位；缩容不取消正在处理的任务。
		for s.ctx.Err() == nil && s.tryAcquireSlot() {
			claimCtx, cancel := context.WithTimeout(s.ctx, persistenceTimeout)
			job, err := s.repo.Claim(claimCtx, s.config.EffectiveMode() != "off")
			cancel()
			if err != nil {
				s.active.Add(-1)
				if s.ctx.Err() == nil {
					s.noteError("job_claim_failed", err)
				}
				break
			}
			if job == nil {
				s.active.Add(-1)
				break
			}
			logger.LegacyPrintf("third_party_prompt_audit", "开始处理审核任务 job_id=%d evaluation_round=%d claim_generation=%d", job.ID, job.Attempts, job.ClaimGeneration)
			tasks.Add(1)
			go func(job *Job) {
				defer tasks.Done()
				defer s.releaseSlot()
				_, _ = s.processJob(s.ctx, job, false)
			}(job)
		}
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *Service) actionLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		claimCtx, cancel := context.WithTimeout(ctx, persistenceTimeout)
		action, err := s.repo.ClaimAction(claimCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.noteError("notification_claim_failed", err)
			}
		} else if action != nil {
			s.processAction(ctx, action)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) processAction(parent context.Context, action *Action) {
	ctx, cancel := context.WithCancelCause(parent)
	done := s.heartbeat(ctx, cancel, func(ctx context.Context) error { return s.repo.RenewAction(ctx, action) })
	defer func() { cancel(nil); <-done }()
	if action.ExecutionStatus != "succeeded" {
		if !s.executeAccountAction(ctx, action) || action.ActionType == "counter_reset" {
			return
		}
	}
	for i := range action.Deliveries {
		delivery := &action.Deliveries[i]
		if delivery.Status == "sent" || delivery.Status == "failed" {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if _, err := mail.ParseAddress(delivery.Recipient); err != nil {
			delivery.Status, delivery.LastError = "failed", "收件人邮箱无效"
			continue
		}
		if len(delivery.Attempts) > 0 {
			prior := &delivery.Attempts[len(delivery.Attempts)-1]
			if prior.FinishedAt == nil {
				now := time.Now().UTC()
				prior.FinishedAt = &now
				prior.Status = "unknown"
				prior.Error = "上次投递未完成确认，结果未知"
			}
		}
		started := time.Now().UTC()
		delivery.Status = "sending"
		delivery.Attempts = append(delivery.Attempts, DeliveryAttempt{StartedAt: started, Status: "started"})
		if err := s.repo.SaveAction(ctx, action, false); err != nil {
			s.noteError("notification_attempt_persist_failed", err)
			return
		}
		logger.LegacyPrintf("third_party_prompt_audit", "开始投递审核通知 action_id=%d recipient=%s", action.ID, delivery.Recipient)
		err := s.email.SendEmail(ctx, delivery.Recipient, delivery.Subject, delivery.Body)
		finished := time.Now().UTC()
		attempt := &delivery.Attempts[len(delivery.Attempts)-1]
		attempt.FinishedAt = &finished
		if err == nil {
			delivery.Status, delivery.LastError = "sent", ""
			attempt.Status = "succeeded"
		} else {
			delivery.Status, delivery.LastError = "pending", err.Error()
			attempt.Status, attempt.Error = "failed", err.Error()
			var smtpErr *textproto.Error
			if errors.Is(err, notify.ErrNotConfigured) || (errors.As(err, &smtpErr) && smtpErr.Code >= 500) {
				delivery.Status = "failed"
			}
			s.noteError("notification_delivery_failed", err)
		}
		persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
		if err := s.repo.SaveAction(persistCtx, action, false); err != nil {
			persistCancel()
			s.noteError("notification_result_persist_failed", err)
			return
		}
		persistCancel()
		logger.LegacyPrintf("third_party_prompt_audit", "审核通知投递结束 action_id=%d recipient=%s status=%s error=%s", action.ID, delivery.Recipient, delivery.Status, delivery.LastError)
	}
	action.NotificationStatus = "sent"
	if len(action.Deliveries) == 0 {
		action.NotificationStatus = "not_required"
	}
	pending, failed, maxAttempts := false, false, 0
	for _, delivery := range action.Deliveries {
		if delivery.Status == "pending" || delivery.Status == "sending" {
			pending = true
		}
		if delivery.Status == "failed" {
			failed = true
		}
		maxAttempts = max(maxAttempts, len(delivery.Attempts))
	}
	if pending {
		action.NotificationStatus = "retry"
	} else if failed {
		action.NotificationStatus = "failed"
	}
	delay := time.Second * time.Duration(1<<min(maxAttempts, 8))
	if !pending {
		delay = time.Second
	}
	action.NextAttemptAt = time.Now().UTC().Add(delay)
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	defer persistCancel()
	if err := s.repo.SaveAction(persistCtx, action, true); err != nil {
		s.noteError("notification_result_persist_failed", err)
	}
}
