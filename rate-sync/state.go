package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const currentStateVersion = 5

type State struct {
	Version       int                          `json:"version"`
	Rules         map[string]*RuleState        `json:"rules"`
	DynamicGroups map[int64]*DynamicGroupState `json:"dynamic_groups,omitempty"`
}

type DynamicGroupState struct {
	Initialized      bool              `json:"initialized"`
	LastUsageID      int64             `json:"last_usage_id"`
	Fast             DynamicCostMemory `json:"fast"`
	Slow             DynamicCostMemory `json:"slow"`
	LastAccountRates map[int64]float64 `json:"last_account_rates,omitempty"`
	PendingTarget    float64           `json:"pending_target,omitempty"`
	HasPendingTarget bool              `json:"has_pending_target,omitempty"`
}

type DynamicCostMemory struct {
	Denominator float64           `json:"denominator"`
	AccountBase map[int64]float64 `json:"account_base,omitempty"`
	AccountCost float64           `json:"account_cost,omitempty"`
}

type RuleState struct {
	Identity              string  `json:"identity"`
	Template              string  `json:"template,omitempty"`
	PriceKey              string  `json:"price_key,omitempty"`
	Day                   string  `json:"day"`
	Cost                  float64 `json:"cost"`
	ActualCost            float64 `json:"actual_cost"`
	HasBaseline           bool    `json:"has_baseline"`
	CandidateUpstreamRate float64 `json:"candidate_upstream_rate,omitempty"`
	CandidateCount        int     `json:"candidate_count,omitempty"`
}

func (s *RuleState) resetCandidate() {
	s.CandidateUpstreamRate = 0
	s.CandidateCount = 0
}

func (s *RuleState) resetUsage() {
	s.Day = ""
	s.Cost = 0
	s.ActualCost = 0
	s.HasBaseline = false
	s.resetCandidate()
}

func (s *RuleState) resetPriceKey() {
	s.PriceKey = ""
	s.resetCandidate()
}

func (s *RuleState) resetTemplate() {
	s.Template = ""
	s.resetUsage()
	s.PriceKey = ""
}

type StateStore struct {
	Path string
}

func (s StateStore) Load() (*State, error) {
	file, err := os.Open(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return newState(), nil
		}
		return nil, fmt.Errorf("打开状态文件: %w", err)
	}
	defer file.Close()

	var state State
	if err := json.NewDecoder(io.LimitReader(file, 1024*1024)).Decode(&state); err != nil {
		return nil, fmt.Errorf("解析状态文件: %w", err)
	}
	if err := validateStateEntries(&state); err != nil {
		return nil, err
	}
	switch state.Version {
	case 2:
		for _, rule := range state.Rules {
			if rule.Template != templateNewAPIRatio {
				continue
			}
			rule.resetPriceKey()
		}
		state.DynamicGroups = make(map[int64]*DynamicGroupState)
		state.Version = currentStateVersion
	case 3:
		state.DynamicGroups = make(map[int64]*DynamicGroupState)
		state.Version = currentStateVersion
	case 4:
		// 动态算法 5 改为以近期观测成本为主；旧状态的长记忆口径不同，重新从近期成功请求初始化。
		state.DynamicGroups = make(map[int64]*DynamicGroupState)
		state.Version = currentStateVersion
	case currentStateVersion:
	default:
		return nil, fmt.Errorf("状态文件版本不支持: %d", state.Version)
	}
	if state.Rules == nil {
		state.Rules = make(map[string]*RuleState)
	}
	if state.DynamicGroups == nil {
		state.DynamicGroups = make(map[int64]*DynamicGroupState)
	}
	return &state, nil
}

func validateStateEntries(state *State) error {
	for key, rule := range state.Rules {
		if rule == nil {
			return fmt.Errorf("状态文件 rules[%q] 不能为空", key)
		}
	}
	for groupID, group := range state.DynamicGroups {
		if group == nil {
			return fmt.Errorf("状态文件 dynamic_groups[%d] 不能为空", groupID)
		}
	}
	return nil
}

func (s StateStore) Save(state *State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("编码状态文件: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("创建状态目录: %w", err)
	}
	tempPath := s.Path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		return fmt.Errorf("写入状态文件: %w", err)
	}
	if err := os.Rename(tempPath, s.Path); err != nil {
		return fmt.Errorf("替换状态文件: %w", err)
	}
	return nil
}

func newState() *State {
	return &State{
		Version:       currentStateVersion,
		Rules:         make(map[string]*RuleState),
		DynamicGroups: make(map[int64]*DynamicGroupState),
	}
}
