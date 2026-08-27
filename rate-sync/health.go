package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const syncHealthFileName = ".health.json"

type syncHealth struct {
	Phase         string    `json:"phase"`
	LastCycleAt   time.Time `json:"last_cycle_at"`
	LastSuccessAt time.Time `json:"last_success_at,omitempty"`
}

const (
	syncHealthWaiting = "waiting_for_admin_key"
	syncHealthHealthy = "healthy"
	syncHealthFailed  = "failed"
)

func syncHealthPath(stateFile string) string {
	return filepath.Join(filepath.Dir(stateFile), syncHealthFileName)
}

func writeSyncHealth(stateFile string, now time.Time) error {
	return writeSyncHealthState(stateFile, syncHealth{
		Phase:         syncHealthHealthy,
		LastCycleAt:   now.UTC(),
		LastSuccessAt: now.UTC(),
	})
}

func writeSyncHealthState(stateFile string, health syncHealth) error {
	path := syncHealthPath(stateFile)
	data, err := json.Marshal(health)
	if err != nil {
		return fmt.Errorf("编码健康状态: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建健康状态目录: %w", err)
	}
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		return fmt.Errorf("写入健康状态: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("替换健康状态: %w", err)
	}
	return nil
}

func readSyncHealth(stateFile string) (syncHealth, error) {
	file, err := os.Open(syncHealthPath(stateFile))
	if err != nil {
		return syncHealth{}, fmt.Errorf("健康状态不可用: %w", err)
	}
	defer file.Close()
	var health syncHealth
	if err := json.NewDecoder(io.LimitReader(file, 16*1024)).Decode(&health); err != nil {
		return syncHealth{}, fmt.Errorf("解析健康状态: %w", err)
	}
	return health, nil
}

func invalidateSyncHealth(stateFile string) error {
	if err := os.Remove(syncHealthPath(stateFile)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清理旧健康状态: %w", err)
	}
	return nil
}

func checkSyncHealth(stateFile string, interval time.Duration, now time.Time) error {
	if interval <= 0 {
		return fmt.Errorf("健康检查周期无效")
	}
	health, err := readSyncHealth(stateFile)
	if err != nil {
		return err
	}
	if health.Phase != "" && health.Phase != syncHealthWaiting && health.Phase != syncHealthHealthy && health.Phase != syncHealthFailed {
		return fmt.Errorf("健康状态阶段无效")
	}
	maxAge := interval * 3
	if maxAge < time.Minute {
		maxAge = time.Minute
	}
	if health.Phase == syncHealthWaiting {
		if health.LastCycleAt.IsZero() {
			return fmt.Errorf("尚未完成同步周期")
		}
		if health.LastCycleAt.After(now.Add(5*time.Minute)) || now.Sub(health.LastCycleAt) >= maxAge {
			return fmt.Errorf("最近同步周期已过期")
		}
		return nil
	}
	if health.Phase == syncHealthFailed {
		if health.LastCycleAt.IsZero() {
			return fmt.Errorf("最近失败周期无效")
		}
		if health.LastCycleAt.After(now.Add(5 * time.Minute)) {
			return fmt.Errorf("最近失败周期无效")
		}
		if !health.LastSuccessAt.IsZero() && health.LastSuccessAt.After(health.LastCycleAt) {
			return fmt.Errorf("健康状态时间顺序无效")
		}
	}
	if health.LastSuccessAt.IsZero() {
		return fmt.Errorf("尚未完成同步周期")
	}
	if health.LastSuccessAt.After(now.Add(5*time.Minute)) || now.Sub(health.LastSuccessAt) >= maxAge {
		return fmt.Errorf("最近同步周期已过期")
	}
	return nil
}
