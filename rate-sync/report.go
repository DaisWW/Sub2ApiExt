package main

import (
	"fmt"
	"strings"
	"sync"
	"unicode"
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

func (r *syncReport) tableLines() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := append([]string(nil), r.order...)
	if len(keys) == 0 {
		return nil
	}
	headers := []string{"账号", "分组", "账户倍率", "分组倍率", "代理", "历史窗口/说明", "结果"}
	rows := make([][]string, 0, len(keys))
	for _, key := range keys {
		row := r.rows[key]
		if row == nil {
			continue
		}
		evidence := row.window
		if row.detail != "" {
			if evidence != "" {
				evidence += "；"
			}
			evidence += row.detail
		}
		rows = append(rows, []string{
			tableCell(row.accountName),
			tableCell(row.groupName),
			fmt.Sprintf("%.4f", row.accountRate),
			fmt.Sprintf("%.4f", row.groupRate),
			tableCell(row.proxy),
			tableCell(evidence),
			tableCell(row.status),
		})
	}

	widths := make([]int, len(headers))
	for index, header := range headers {
		widths[index] = displayWidth(header)
	}
	for _, row := range rows {
		for index, cell := range row {
			if width := displayWidth(cell); width > widths[index] {
				widths[index] = width
			}
		}
	}

	lines := make([]string, 0, len(rows)+2)
	lines = append(lines, formatTableRow(headers, widths, nil))
	lines = append(lines, formatTableSeparator(widths))
	for _, row := range rows {
		lines = append(lines, formatTableRow(row, widths, map[int]bool{2: true, 3: true}))
	}
	return lines
}

func formatTableRow(cells []string, widths []int, rightAligned map[int]bool) string {
	parts := make([]string, len(cells))
	for index, cell := range cells {
		padding := widths[index] - displayWidth(cell)
		if rightAligned != nil && rightAligned[index] {
			parts[index] = strings.Repeat(" ", padding) + cell
		} else {
			parts[index] = cell + strings.Repeat(" ", padding)
		}
	}
	return strings.TrimRight(strings.Join(parts, "  "), " ")
}

func formatTableSeparator(widths []int) string {
	parts := make([]string, len(widths))
	for index, width := range widths {
		parts[index] = strings.Repeat("-", width)
	}
	return strings.Join(parts, "  ")
}

func displayWidth(value string) int {
	width := 0
	for _, character := range value {
		switch {
		case character == '\t':
			width += 4
		case unicode.Is(unicode.Mn, character):
			// Combining marks do not consume an extra terminal cell.
		case isWideDisplayRune(character):
			width += 2
		default:
			width++
		}
	}
	return width
}

func isWideDisplayRune(character rune) bool {
	return character >= 0x1100 && (character <= 0x115f ||
		character == 0x2329 || character == 0x232a ||
		(character >= 0x2e80 && character <= 0xa4cf && character != 0x303f) ||
		(character >= 0xac00 && character <= 0xd7a3) ||
		(character >= 0xf900 && character <= 0xfaff) ||
		(character >= 0xfe10 && character <= 0xfe19) ||
		(character >= 0xfe30 && character <= 0xfe6f) ||
		(character >= 0xff00 && character <= 0xff60) ||
		(character >= 0xffe0 && character <= 0xffe6) ||
		(character >= 0x1f300 && character <= 0x1faff))
}

func tableCell(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	if value == "" {
		return "-"
	}
	return value
}

func (s *Syncer) logSyncReport(report *syncReport) {
	if s == nil || s.logger == nil || report == nil {
		return
	}
	lines := report.tableLines()
	if len(lines) == 0 {
		return
	}
	s.logger.Printf("[TABLE] 同步倍率汇总（目标=%s，绑定=%d）", report.target, len(report.rows))
	for _, line := range lines {
		s.logger.Printf("[TABLE] %s", line)
	}
}
