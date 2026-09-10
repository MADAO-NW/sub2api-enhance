package thirdpartypromptaudit

import (
	"context"
	"database/sql"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// latencySampleCapacity 限定实例诊断样本内存，业务流水仍完整保存在数据库。
const latencySampleCapacity = 1000

type LatencySummary struct {
	Count    int        `json:"count"`
	Capacity int        `json:"capacity"`
	From     *time.Time `json:"from"`
	To       *time.Time `json:"to"`
	P50MS    *float64   `json:"p50_ms"`
	P95MS    *float64   `json:"p95_ms"`
}

type RuntimeProblem struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

type latencySample struct {
	Started, Finished time.Time
	Milliseconds      float64
}

type RuntimeMetrics struct {
	InstanceID     string
	StartedAt      time.Time
	IntakeFailures atomic.Uint64
	mu             sync.Mutex
	samples        []latencySample
	nextSample     int
	lastSuccess    *time.Time
	lastError      *RuntimeProblem
	probes         map[string]ProbeResult
}

func NewRuntimeMetrics() *RuntimeMetrics {
	return &RuntimeMetrics{InstanceID: uuid.NewString(), StartedAt: time.Now().UTC(), samples: make([]latencySample, 0, latencySampleCapacity), probes: map[string]ProbeResult{}}
}

func (m *RuntimeMetrics) ObserveEvaluation(started, finished time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sample := latencySample{Started: started, Finished: finished, Milliseconds: float64(finished.Sub(started)) / float64(time.Millisecond)}
	if len(m.samples) < latencySampleCapacity {
		m.samples = append(m.samples, sample)
	} else {
		m.samples[m.nextSample] = sample
		m.nextSample = (m.nextSample + 1) % latencySampleCapacity
	}
}

func (m *RuntimeMetrics) Success(at time.Time) { m.mu.Lock(); defer m.mu.Unlock(); m.lastSuccess = &at }
func (m *RuntimeMetrics) Error(code, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastError = &RuntimeProblem{Code: code, Message: message, At: time.Now().UTC()}
}
func (m *RuntimeMetrics) Probe(result ProbeResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.probes[result.ModelID] = result
}

type RuntimeSnapshot struct {
	CapturePersistFailures uint64                   `json:"capture_persist_failures"`
	IngressPool            sql.DBStats              `json:"ingress_pool"`
	WorkerPool             sql.DBStats              `json:"worker_pool"`
	WarningEnabled         bool                     `json:"warning_enabled"`
	DisableEnabled         bool                     `json:"disable_enabled"`
	InstanceID             string                   `json:"instance_id"`
	StartedAt              time.Time                `json:"started_at"`
	AsOf                   time.Time                `json:"as_of"`
	Running                bool                     `json:"running"`
	Mode                   string                   `json:"mode"`
	Revision               int64                    `json:"revision"`
	ExpectedRevision       int64                    `json:"expected_revision"`
	WorkerCapacity         int                      `json:"worker_capacity"`
	ActiveWorkers          int64                    `json:"active_workers"`
	DatabaseOK             bool                     `json:"database_ok"`
	DatabaseError          string                   `json:"database_error"`
	ConfigError            string                   `json:"config_error"`
	InputPersistFailures   uint64                   `json:"input_persist_failures"`
	LastSuccess            *time.Time               `json:"last_success"`
	LastError              *RuntimeProblem          `json:"last_error"`
	EvaluationLatency      LatencySummary           `json:"evaluation_latency"`
	Probes                 []ProbeResult            `json:"probes"`
	Scheduler              schedulerRuntimeSnapshot `json:"scheduler"`
}

func (s *Service) Runtime(ctx context.Context) RuntimeSnapshot {
	result := RuntimeSnapshot{InstanceID: s.metrics.InstanceID, StartedAt: s.metrics.StartedAt, AsOf: time.Now().UTC(), Running: s.running.Load(), Mode: s.config.EffectiveMode(),
		ActiveWorkers: s.active.Load(), InputPersistFailures: s.metrics.IntakeFailures.Load(), Probes: []ProbeResult{}}
	result.WorkerCapacity = s.config.WorkerCapacity()
	result.WorkerPool = s.repo.db.Stats()
	s.config.mu.RLock()
	result.ExpectedRevision = s.config.expectedRevision
	if s.config.active != nil {
		result.Revision = s.config.active.Stored.Revision
		result.WarningEnabled = s.config.active.Stored.Warning.Enabled
		result.DisableEnabled = s.config.active.Stored.Disable.Enabled
		for _, rule := range s.config.active.Stored.UserRules {
			result.WarningEnabled = result.WarningEnabled || rule.Warning.Enabled
			result.DisableEnabled = result.DisableEnabled || rule.Disable.Enabled
		}
	}
	if s.config.loadError != nil {
		result.ConfigError = s.config.loadError.Error()
	}
	s.config.mu.RUnlock()
	if snapshot, err := s.config.Active(); err == nil {
		result.Scheduler = s.evaluator.nodeScheduler().runtime(snapshot.Models)
	} else {
		result.Scheduler = schedulerRuntimeSnapshot{Nodes: []nodeRuntimeSnapshot{}}
	}

	dbCtx, cancel := context.WithTimeout(ctx, persistenceTimeout)
	err := s.repo.db.PingContext(dbCtx)
	cancel()
	result.DatabaseOK = err == nil
	if err != nil {
		result.DatabaseError = err.Error()
	}
	s.metrics.mu.Lock()
	defer s.metrics.mu.Unlock()
	result.LastSuccess = s.metrics.lastSuccess
	result.LastError = s.metrics.lastError
	for _, probe := range s.metrics.probes {
		result.Probes = append(result.Probes, probe)
	}
	slices.SortFunc(result.Probes, func(a, b ProbeResult) int { return a.TestedAt.Compare(b.TestedAt) })
	summary := LatencySummary{Count: len(s.metrics.samples), Capacity: latencySampleCapacity}
	if len(s.metrics.samples) > 0 {
		values := make([]float64, 0, len(s.metrics.samples))
		from, to := s.metrics.samples[0].Started, s.metrics.samples[0].Finished
		for _, sample := range s.metrics.samples {
			values = append(values, sample.Milliseconds)
			if sample.Started.Before(from) {
				from = sample.Started
			}
			if sample.Finished.After(to) {
				to = sample.Finished
			}
		}
		slices.Sort(values)
		p50 := values[int(math.Ceil(float64(len(values))*0.50))-1]
		p95 := values[int(math.Ceil(float64(len(values))*0.95))-1]
		summary.From, summary.To, summary.P50MS, summary.P95MS = &from, &to, &p50, &p95
	}
	result.EvaluationLatency = summary
	return result
}
