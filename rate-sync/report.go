package main

import (
	"fmt"
	"strings"
	"sync"
)

const (
	reportStatusPending = "未处理"
	reportStatusChecked = "检查成功"
	reportStatusStable  = "稳定"
	reportStatusPreview = "预览"
	reportStatusUpdated = "已更新"
	reportStatusSkipped = "暂不自动"
	reportStatusFailed  = "失败"
)

type syncReport struct {
	target string
	mu     sync.Mutex
	rows   map[string]*syncReportRow
	order  []string
}

type syncReportRow struct {
	key         string
	accountID   int64
	groupID     int64
	accountName string
	groupName   string
	accountRate float64
	groupRate   float64
	proxy       string
	status      string
	window      string
	detail      string
}

func newSyncReport(target string, channels []Channel) *syncReport {
	report := &syncReport{
		target: target,
		rows:   make(map[string]*syncReportRow, len(channels)),
		order:  make([]string, 0, len(channels)),
	}
	for i := range channels {
		channel := &channels[i]
		key := fmt.Sprintf("account:%d/group:%d", channel.AccountID, channel.Group.ID)
		if _, exists := report.rows[key]; exists {
			continue
		}
		report.rows[key] = &syncReportRow{
			key:         key,
			accountID:   channel.AccountID,
			groupID:     channel.Group.ID,
			accountName: strings.TrimSpace(channel.AccountName),
			groupName:   strings.TrimSpace(channel.Group.Name),
			accountRate: channel.AccountRateMultiplier,
			groupRate:   channel.Group.RateMultiplier,
			proxy:       proxyLabel(channel.ProxyURL),
			status:      reportStatusPending,
		}
		report.order = append(report.order, key)
	}
	return report
}

func (r *syncReport) markChannel(channel *Channel, status string) {
	if r == nil || channel == nil {
		return
	}
	if r.target == "account" {
		r.markAccount(channel.AccountID, status)
		return
	}
	r.markGroup(channel.Group.ID, status)
}

func (r *syncReport) markAccount(accountID int64, status string) {
	r.mark(func(row *syncReportRow) bool {
		return strings.HasPrefix(row.key, fmt.Sprintf("account:%d/", accountID))
	}, status)
}

func (r *syncReport) markGroup(groupID int64, status string) {
	suffix := fmt.Sprintf("/group:%d", groupID)
	r.mark(func(row *syncReportRow) bool {
		return strings.HasSuffix(row.key, suffix)
	}, status)
}

func (r *syncReport) mark(predicate func(*syncReportRow) bool, status string) {
	if r == nil || status == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range r.rows {
		if predicate(row) && reportStatusRank(status) >= reportStatusRank(row.status) {
			row.status = status
		}
	}
}

func (r *syncReport) updateAccountRate(accountID int64, rate float64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := fmt.Sprintf("account:%d/", accountID)
	for _, row := range r.rows {
		if strings.HasPrefix(row.key, prefix) {
			row.accountRate = rate
		}
	}
}

func (r *syncReport) updateGroupRate(groupID int64, rate float64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	suffix := fmt.Sprintf("/group:%d", groupID)
	for _, row := range r.rows {
		if strings.HasSuffix(row.key, suffix) {
			row.groupRate = rate
		}
	}
}

func (r *syncReport) setGroupEvidence(groupID int64, window, detail string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	suffix := fmt.Sprintf("/group:%d", groupID)
	for _, row := range r.rows {
		if strings.HasSuffix(row.key, suffix) {
			row.window = strings.TrimSpace(window)
			row.detail = strings.TrimSpace(detail)
		}
	}
}

func reportStatusRank(status string) int {
	switch status {
	case reportStatusFailed:
		return 6
	case reportStatusSkipped:
		return 5
	case reportStatusUpdated:
		return 4
	case reportStatusPreview:
		return 3
	case reportStatusStable:
		return 2
	case reportStatusChecked:
		return 1
	default:
		return 0
	}
}

func (s *Syncer) logSyncReport(report *syncReport) {
	if s == nil || s.logger == nil || report == nil {
		return
	}
	lines := report.tableLines()
	if len(lines) == 0 {
		return
	}
	title := "分组倍率同步"
	countLabel := "分组数"
	if report.target == "account" {
		title = "账户倍率同步"
		countLabel = "账号数"
	}
	s.logger.Printf("[TABLE] %s（%s=%d）", title, countLabel, report.summaryCount())
	for _, line := range lines {
		s.logger.Printf("[TABLE] %s", line)
	}
}

func (r *syncReport) summaryCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[int64]struct{})
	for _, key := range r.order {
		row := r.rows[key]
		if row == nil {
			continue
		}
		if r.target == "account" {
			seen[row.accountID] = struct{}{}
		} else {
			seen[row.groupID] = struct{}{}
		}
	}
	return len(seen)
}

// healthy reports whether this cycle has enough successful evidence to refresh
// the process health marker. An empty discovery result is healthy idle work;
// a skipped-only or failed cycle is not healthy.
func (r *syncReport) healthy() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rows) == 0 {
		return true
	}
	hasSuccess := false
	for _, row := range r.rows {
		switch row.status {
		case reportStatusChecked, reportStatusStable, reportStatusPreview, reportStatusUpdated:
			hasSuccess = true
		}
	}
	return hasSuccess
}
