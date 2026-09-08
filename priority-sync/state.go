package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type syncState struct {
	Accounts map[int64]accountState `json:"accounts"`
}

type accountState struct {
	CandidatePriority int        `json:"candidate_priority,omitempty"`
	CandidateCount    int        `json:"candidate_count,omitempty"`
	LastAppliedAt     *time.Time `json:"last_applied_at,omitempty"`
	LastApplied       int        `json:"last_applied_priority,omitempty"`
}

func loadState(path string) (*syncState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &syncState{Accounts: make(map[int64]accountState)}, nil
		}
		return nil, fmt.Errorf("read priority state: %w", err)
	}
	var state syncState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode priority state: %w", err)
	}
	if state.Accounts == nil {
		state.Accounts = make(map[int64]accountState)
	}
	return &state, nil
}

func saveState(path string, state *syncState) error {
	if state == nil {
		return fmt.Errorf("priority state is nil")
	}
	if state.Accounts == nil {
		state.Accounts = make(map[int64]accountState)
	}
	return writeJSONAtomic(path, state)
}

func saveReport(path string, report PriorityReport) error {
	return writeJSONAtomic(path, report)
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode priority file: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create priority state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".priority-*.tmp")
	if err != nil {
		return fmt.Errorf("create priority temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = os.Remove(temporaryName)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect priority temporary file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write priority temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync priority temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close priority temporary file: %w", err)
	}
	if err := os.Chmod(temporaryName, 0o600); err != nil {
		return fmt.Errorf("protect priority file: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace priority file: %w", err)
	}
	return nil
}
