package main

import (
	"fmt"
	"math"
	"time"
)

type upstreamToday struct {
	Cost       float64 `json:"cost"`
	ActualCost float64 `json:"actual_cost"`
}

type upstreamResponse struct {
	Usage *struct {
		Today *upstreamToday `json:"today"`
	} `json:"usage"`
}

func (s *Syncer) syncTarget() string {
	if s == nil || s.config == nil || s.config.SyncTarget == "" {
		return defaultSyncTarget
	}
	return s.config.SyncTarget
}

func (s *Syncer) localRate(channel *Channel) float64 {
	if s.syncTarget() == "account" {
		return channel.AccountRateMultiplier
	}
	return channel.Group.RateMultiplier
}

func cumulativeUsageRate(current upstreamToday) (float64, bool) {
	if current.Cost <= 0 || current.ActualCost <= 0 {
		return 0, false
	}
	rate := round4(current.ActualCost / current.Cost)
	return rate, validPositiveRate(rate)
}

func (s *Syncer) observeUsage(name string, state *RuleState, current upstreamToday, localRate float64, now time.Time) {
	day := now.Format("2006-01-02")
	if !state.HasBaseline {
		s.initializeUsageBaseline(name, state, current, localRate, day)
		return
	}
	if state.Day != day {
		s.resetUsageBaseline(name, state, current, localRate, day)
		return
	}
	if usageCounterRegressed(state, current) {
		state.resetCandidate()
		setBaseline(state, day, current)
		if s.observeCumulativeUsage(name, state, current, "上游累计值下降，已重置用量基线") {
			return
		}
		s.logger.Printf("[%s] 上游累计值下降，已重置用量基线；当前本地使用倍率 %.4f，等待有效请求成本", name, localRate)
		return
	}

	deltaCost := current.Cost - state.Cost
	deltaActual := current.ActualCost - state.ActualCost
	setBaseline(state, day, current)
	if deltaCost <= 1e-12 {
		if s.observeCumulativeUsage(name, state, current, "无新增用量") {
			return
		}
		s.logger.Printf("[%s] 检查: 无新增用量，当前本地使用倍率 %.4f，等待有效请求成本", name, localRate)
		return
	}
	if deltaActual <= 0 {
		if s.observeCumulativeUsage(name, state, current, "新增用量尚未产生实际成本") {
			return
		}
		state.resetCandidate()
		s.logger.Printf("[%s] 新用量无法计算出正倍率，本轮跳过；当前本地使用倍率 %.4f", name, localRate)
		return
	}
	s.observeDeltaRate(name, state, deltaActual/deltaCost, localRate)
}

func (s *Syncer) initializeUsageBaseline(name string, state *RuleState, current upstreamToday, localRate float64, day string) {
	state.resetCandidate()
	setBaseline(state, day, current)
	if s.observeCumulativeUsage(name, state, current, "已建立上游用量基线") {
		return
	}
	s.logger.Printf("[%s] 已建立上游用量基线，当前本地使用倍率 %.4f，等待有效请求成本", name, localRate)
}

func (s *Syncer) resetUsageBaseline(name string, state *RuleState, current upstreamToday, localRate float64, day string) {
	state.resetCandidate()
	setBaseline(state, day, current)
	if s.observeCumulativeUsage(name, state, current, "日期已变化，已重置用量基线") {
		return
	}
	s.logger.Printf("[%s] 日期已变化，已重置用量基线；当前本地使用倍率 %.4f，等待有效请求成本", name, localRate)
}

func usageCounterRegressed(state *RuleState, current upstreamToday) bool {
	return current.Cost < state.Cost || current.ActualCost < state.ActualCost
}

func (s *Syncer) observeCumulativeUsage(name string, state *RuleState, current upstreamToday, reason string) bool {
	upstreamRate, ok := cumulativeUsageRate(current)
	if !ok {
		return false
	}
	s.observeRate(name, state, upstreamRate)
	s.logger.Printf(
		"[%s] %s，使用累计请求成本计算上游倍率 %.4f（确认 %d/%d）",
		name, reason, upstreamRate, state.CandidateCount, s.config.Confirmations,
	)
	return true
}

func (s *Syncer) observeDeltaRate(name string, state *RuleState, rawRate, localRate float64) {
	upstreamRate := round4(rawRate)
	if !validPositiveRate(upstreamRate) {
		state.resetCandidate()
		s.logger.Printf("[%s] 上游倍率无效，本轮跳过；当前本地使用倍率 %.4f", name, localRate)
		return
	}
	s.observeRate(name, state, upstreamRate)
}

func (s *Syncer) observeRate(name string, state *RuleState, upstreamRate float64) {
	state.CandidateCount = min(state.CandidateCount, s.config.Confirmations)
	if almostEqual(state.CandidateUpstreamRate, upstreamRate) {
		if state.CandidateCount < s.config.Confirmations {
			state.CandidateCount++
		}
	} else {
		state.CandidateUpstreamRate = upstreamRate
		state.CandidateCount = 1
	}
	s.logger.Printf("[%s] 检测到上游倍率 %.4f（确认 %d/%d）", name, upstreamRate, state.CandidateCount, s.config.Confirmations)
}

func setBaseline(state *RuleState, day string, current upstreamToday) {
	state.Day = day
	state.Cost = current.Cost
	state.ActualCost = current.ActualCost
	state.HasBaseline = true
}

func validateUsage(today *upstreamToday) error {
	if today.Cost < 0 || today.ActualCost < 0 ||
		math.IsNaN(today.Cost) || math.IsNaN(today.ActualCost) ||
		math.IsInf(today.Cost, 0) || math.IsInf(today.ActualCost, 0) {
		return fmt.Errorf("上游 usage.today 包含无效数值")
	}
	return nil
}
