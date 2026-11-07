package thirdpartypromptaudit

import (
	"context"
	"time"

	infraerrors "sub2api-enhance/internal/pkg/errors"
	"sub2api-enhance/internal/pkg/logger"
)

// PolicyReplayRequest 是政策回放请求：草稿配置加上样本选择（为空表示全部样本）。
type PolicyReplayRequest struct {
	Config    Config  `json:"config"`
	SampleIDs []int64 `json:"sample_ids"`
}

// StartPolicyReplay 校验草稿配置后创建回放记录并在后台逐样本执行。
// 回放不建任务、不写 prompt_reviews、不进缓存、不触发处置；全局 off 下同样允许运行。
func (s *Service) StartPolicyReplay(ctx context.Context, request PolicyReplayRequest, actorID int64) (*PolicyReplay, error) {
	if err := validateConfig(request.Config, true); err != nil {
		return nil, infraerrors.BadRequest("third_party_audit_invalid_config", err.Error())
	}
	ids, err := s.repo.sampleIDsForReplay(ctx, request.SampleIDs)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, infraerrors.BadRequest("third_party_audit_no_samples", "请先添加误判样本，再执行政策回放")
	}
	replay, err := s.repo.CreateReplay(ctx, request.Config, ids, actorID)
	if err != nil {
		return nil, infraerrors.Conflict("third_party_audit_replay_running", err.Error())
	}
	s.wg.Add(1)
	go s.runPolicyReplay(replay.ID, request.Config, ids)
	return replay, nil
}

// runPolicyReplay 逐个样本执行回放；每个样本对全部启用节点各调用一次。
func (s *Service) runPolicyReplay(replayID int64, config Config, sampleIDs []int64) {
	defer s.wg.Done()
	// 回放是管理员手动调用，不随请求取消；服务关闭时 ctx 结束。
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	samples, err := s.repo.SampleTexts(ctx, sampleIDs)
	if err != nil {
		s.finishReplay(replayID, ReplayFailed, nil, err.Error())
		return
	}
	results := make([]PolicyReplayResult, 0, len(samples))
	for _, sample := range samples {
		if err := ctx.Err(); err != nil {
			s.finishReplay(replayID, ReplayInterrupted, results, "服务停止，回放已中断")
			return
		}
		results = append(results, s.replaySample(ctx, replayID, config, sample))
		if err := s.repo.SaveReplayResults(ctx, replayID, ReplayRunning, results, ""); err != nil {
			logger.LegacyPrintf("third_party_prompt_audit", "政策回放进度保存失败 replay_id=%d error=%v", replayID, err)
		}
	}
	s.finishReplay(replayID, ReplayDone, results, "")
	logger.LegacyPrintf("third_party_prompt_audit", "政策回放完成 replay_id=%d samples=%d", replayID, len(results))
}

// finishReplay 写入回放终态。
func (s *Service) finishReplay(replayID int64, status string, results []PolicyReplayResult, message string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), persistenceTimeout)
	defer cancel()
	if err := s.repo.SaveReplayResults(ctx, replayID, status, results, message); err != nil {
		logger.LegacyPrintf("third_party_prompt_audit", "政策回放状态保存失败 replay_id=%d status=%s error=%v", replayID, status, err)
	}
}

// replaySample 对一个样本执行全部启用节点的真实调用，并按草稿的阈值与聚合得出结论。
func (s *Service) replaySample(ctx context.Context, replayID int64, config Config, sample SampleText) PolicyReplayResult {
	result := PolicyReplayResult{SampleID: sample.ID, Expected: sample.Expected, Categories: []string{}, Nodes: []ReplayNodeResult{}}
	if sample.Text == "" {
		result.Error = "样本正文已不可用"
		return result
	}
	snapshot := ConfigSnapshot{Config: config, ContractVersion: ContractVersion, FixedContract: OutputContract}
	nodes := make([]NodeResult, 0, len(config.Models))
	for _, model := range config.Models {
		if !model.Enabled {
			continue
		}
		node := ReplayNodeResult{ModelID: model.ID, ModelName: model.Name}
		key, err := s.config.ResolveKeyByModelID(model.ID)
		if err != nil || key == "" {
			node.Error = "节点凭据不可用"
			result.Nodes = append(result.Nodes, node)
			continue
		}
		client, url, err := nodeHTTPClient(model)
		if err != nil {
			node.Error = err.Error()
			result.Nodes = append(result.Nodes, node)
			continue
		}
		callCtx, cancel := context.WithTimeout(context.WithValue(ctx, replayContext, replayID), time.Duration(model.TimeoutMS)*time.Millisecond)
		score, _, failure := s.client.EvaluateTarget(callCtx, nil, model, key, snapshot, client, url, sample.Kind, sample.Text, sample.Hash, nil)
		cancel()
		client.CloseIdleConnections()
		if failure != nil {
			node.Error = failure.Message
			result.Nodes = append(result.Nodes, node)
			continue
		}
		confidence := score.Confidence
		node.Score, node.Reason = &confidence, score.Reason
		node.Decision = string(classifyScore(confidence, config))
		result.Nodes = append(result.Nodes, node)
		nodes = append(nodes, NodeResult{ModelID: model.ID, ModelName: model.Name, Confidence: &confidence, Reason: score.Reason})
	}
	if len(nodes) == 0 {
		result.Error = "没有可用的启用节点"
		return result
	}
	decision, _, failure := aggregateVotes(votesFrom(nodes, config), config)
	if failure != nil {
		result.Error = failure.Error()
		return result
	}
	result.Actual = string(decision)
	result.Score = reviewScore(nodes, decision)
	result.Categories = targetCategories(nodes, config, decision)
	if result.Categories == nil {
		result.Categories = []string{}
	}
	return result
}

// ReplayEstimates 返回样本数 × 启用节点数的调用估算，供前端执行前确认。
func (s *Service) ReplayEstimates(ctx context.Context, request PolicyReplayRequest) (map[string]int64, error) {
	ids, err := s.repo.sampleIDsForReplay(ctx, request.SampleIDs)
	if err != nil {
		return nil, err
	}
	enabled := 0
	for _, model := range request.Config.Models {
		if model.Enabled {
			enabled++
		}
	}
	return map[string]int64{"samples": int64(len(ids)), "enabled_nodes": int64(enabled), "planned_calls": int64(len(ids) * enabled)}, nil
}
