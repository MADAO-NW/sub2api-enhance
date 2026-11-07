package thirdpartypromptaudit

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// metricsCacheTTL 限定聚合查询频率，避免 Prometheus 高频抓取压到数据库。
const metricsCacheTTL = 15 * time.Second

// Metrics 输出 Prometheus 文本格式的核心指标（只含计数与健康状态，不含任何正文）。
func (s *Service) PrometheusMetrics(ctx context.Context) string {
	s.metricsMu.Lock()
	if s.metricsCache != "" && time.Since(s.metricsAt) < metricsCacheTTL {
		body := s.metricsCache
		s.metricsMu.Unlock()
		return body
	}
	s.metricsMu.Unlock()

	var builder strings.Builder
	builder.WriteString("# HELP sub2api_enhance_audit_jobs 审核任务数（按状态与裁决）\n")
	builder.WriteString("# TYPE sub2api_enhance_audit_jobs gauge\n")
	rows, err := s.repo.db.QueryContext(ctx, `SELECT status,COALESCE(decision,''),count(*) FROM sub2api_enhance.audit_jobs GROUP BY 1,2`)
	if err == nil {
		for rows.Next() {
			var status, decision string
			var count int64
			if rows.Scan(&status, &decision, &count) == nil {
				fmt.Fprintf(&builder, "sub2api_enhance_audit_jobs{status=%q,decision=%q} %d\n", status, decision, count)
			}
		}
		_ = rows.Close()
	}
	builder.WriteString("# HELP sub2api_enhance_audit_queue 排队中的任务数\n")
	builder.WriteString("# TYPE sub2api_enhance_audit_queue gauge\n")
	var queued int64
	_ = s.repo.db.QueryRowContext(ctx, `SELECT count(*) FROM sub2api_enhance.audit_jobs WHERE status IN ('queued','retry')`).Scan(&queued)
	fmt.Fprintf(&builder, "sub2api_enhance_audit_queue %d\n", queued)
	builder.WriteString("# HELP sub2api_enhance_audit_breakers 熔断中的指令块数\n")
	builder.WriteString("# TYPE sub2api_enhance_audit_breakers gauge\n")
	var breakers, trusted int64
	_ = s.repo.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE status='tripped'),count(*) FILTER (WHERE status='trusted') FROM sub2api_enhance.instruction_breakers`).Scan(&breakers, &trusted)
	fmt.Fprintf(&builder, "sub2api_enhance_audit_breakers %d\n", breakers)
	builder.WriteString("# HELP sub2api_enhance_audit_trusted_blocks 已信任的指令块数\n")
	builder.WriteString("# TYPE sub2api_enhance_audit_trusted_blocks gauge\n")
	fmt.Fprintf(&builder, "sub2api_enhance_audit_trusted_blocks %d\n", trusted)
	builder.WriteString("# HELP sub2api_enhance_audit_workers 同步与后台并发占用\n")
	builder.WriteString("# TYPE sub2api_enhance_audit_workers gauge\n")
	fmt.Fprintf(&builder, "sub2api_enhance_audit_workers{kind=\"async\"} %d\n", s.active.Load())
	fmt.Fprintf(&builder, "sub2api_enhance_audit_workers{kind=\"sync\"} %d\n", s.syncActive.Load())
	builder.WriteString("# HELP sub2api_enhance_audit_config_revision 已应用的配置修订号\n")
	builder.WriteString("# TYPE sub2api_enhance_audit_config_revision gauge\n")
	s.config.mu.RLock()
	fmt.Fprintf(&builder, "sub2api_enhance_audit_config_revision %d\n", s.config.expectedRevision)
	s.config.mu.RUnlock()
	builder.WriteString("# HELP sub2api_enhance_audit_mode 当前审核模式（1 表示生效）\n")
	builder.WriteString("# TYPE sub2api_enhance_audit_mode gauge\n")
	mode := s.config.EffectiveMode()
	for _, candidate := range []string{"off", "async", "blocking"} {
		value := 0
		if candidate == mode {
			value = 1
		}
		fmt.Fprintf(&builder, "sub2api_enhance_audit_mode{mode=%q} %d\n", candidate, value)
	}
	body := builder.String()
	s.metricsMu.Lock()
	s.metricsCache, s.metricsAt = body, time.Now()
	s.metricsMu.Unlock()
	return body
}
