package main

import (
	"fmt"
	"strings"
	"unicode"
)

type reportTableRow struct {
	cells []string
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
	headers, rows := r.summaryRows(keys)
	widths := tableColumnWidths(headers, rows)
	lines := make([]string, 0, len(rows)+2)
	lines = append(lines, formatTableRow(headers, widths))
	lines = append(lines, formatTableSeparator(widths))
	for _, row := range rows {
		lines = append(lines, formatTableRow(row.cells, widths))
	}
	return lines
}

func tableColumnWidths(headers []string, rows []reportTableRow) []int {
	widths := make([]int, len(headers))
	for index, header := range headers {
		widths[index] = displayWidth(header)
	}
	for _, row := range rows {
		for index, cell := range row.cells {
			if width := displayWidth(cell); width > widths[index] {
				widths[index] = width
			}
		}
	}
	return widths
}

func (r *syncReport) summaryRows(keys []string) ([]string, []reportTableRow) {
	if r.target == "account" {
		return r.accountSummaryRows(keys)
	}
	return r.groupSummaryRows(keys)
}

func (r *syncReport) groupSummaryRows(keys []string) ([]string, []reportTableRow) {
	type groupSummary struct {
		name     string
		rate     float64
		statuses []string
		window   string
		detail   string
	}

	summaries := make(map[int64]*groupSummary)
	order := make([]int64, 0, len(keys))
	for _, key := range keys {
		row := r.rows[key]
		if row == nil {
			continue
		}
		summary := summaries[row.groupID]
		if summary == nil {
			summary = &groupSummary{name: row.groupName, rate: row.groupRate}
			summaries[row.groupID] = summary
			order = append(order, row.groupID)
		}
		summary.statuses = appendUnique(summary.statuses, row.status)
		if summary.window == "" && summary.detail == "" {
			summary.window = row.window
			summary.detail = row.detail
		}
	}

	rows := make([]reportTableRow, 0, len(order))
	for _, groupID := range order {
		summary := summaries[groupID]
		rows = append(rows, reportTableRow{
			cells: []string{
				tableCell(summary.name),
				fmt.Sprintf("%.4f", summary.rate),
				tableCell(strings.Join(summary.statuses, ", ")),
				tableCell(reportExplanation(summary.window, summary.detail)),
			},
		})
	}
	return []string{"分组", "分组倍率", "结果", "说明"}, rows
}

func (r *syncReport) accountSummaryRows(keys []string) ([]string, []reportTableRow) {
	type accountSummary struct {
		name             string
		rate             float64
		previousRate     float64
		expectedRate     float64
		hasExpected      bool
		rechargeDiscount float64
		proxies          []string
		statuses         []string
		accountSource    []string
	}

	summaries := make(map[int64]*accountSummary)
	order := make([]int64, 0, len(keys))
	for _, key := range keys {
		row := r.rows[key]
		if row == nil {
			continue
		}
		summary := summaries[row.accountID]
		if summary == nil {
			summary = &accountSummary{
				name:             row.accountName,
				rate:             row.accountRate,
				previousRate:     row.previousRate,
				rechargeDiscount: row.rechargeDiscount,
			}
			summaries[row.accountID] = summary
			order = append(order, row.accountID)
		}
		if !summary.hasExpected && row.hasExpected {
			summary.expectedRate = row.expectedRate
			summary.hasExpected = true
		}
		summary.proxies = appendUnique(summary.proxies, row.proxy)
		summary.statuses = appendUnique(summary.statuses, row.status)
		if row.accountSource != "" {
			summary.accountSource = appendUnique(summary.accountSource, row.accountSource)
		}
	}

	rows := make([]reportTableRow, 0, len(order))
	for _, accountID := range order {
		summary := summaries[accountID]
		rows = append(rows, reportTableRow{
			cells: []string{
				tableCell(summary.name),
				fmt.Sprintf("%.4f", summary.rate),
				accountExpectedRateLabel(summary.expectedRate, summary.hasExpected),
				fmt.Sprintf("%.4f", summary.rechargeDiscount),
				tableCell(accountResultLabel(summary.statuses, summary.accountSource, summary.previousRate)),
				tableCell(strings.Join(summary.proxies, ", ")),
			},
		})
	}
	return []string{"账号", "账户倍率", "预期倍率", "充值折扣", "结果", "代理"}, rows
}

func accountExpectedRateLabel(rate float64, ok bool) string {
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%.4f", rate)
}

func accountResultLabel(statuses, sources []string, previousRate float64) string {
	parts := make([]string, 0, 3)
	if result := strings.Join(statuses, "、"); result != "" {
		parts = append(parts, result)
	}
	if len(sources) > 0 {
		parts = append(parts, strings.Join(sources, "、"))
	}
	if accountResultShowsPrevious(statuses) {
		parts = append(parts, fmt.Sprintf("原 %.4f", previousRate))
	}
	return strings.Join(parts, "｜")
}

func accountResultShowsPrevious(statuses []string) bool {
	for _, status := range statuses {
		switch status {
		case reportStatusUpdated, reportStatusPreview, reportStatusFailed:
			return true
		}
	}
	return false
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func reportExplanation(window, detail string) string {
	window = strings.TrimSpace(window)
	detail = strings.TrimSpace(detail)
	switch {
	case strings.Contains(detail, "样本不足"):
		return "样本不足"
	case strings.Contains(detail, "历史账号成本无效"):
		return "历史成本无效"
	case strings.Contains(detail, "等待账户倍率同步"):
		return "等待账户倍率"
	case strings.Contains(detail, "四舍五入后无效"):
		return "账户倍率无效"
	case strings.Contains(detail, "待发布目标无效"):
		return "目标无效，等待初始化"
	case strings.Contains(detail, "等待重新初始化"):
		return "等待重新初始化"
	case strings.Contains(detail, "计算结果无效"):
		return "计算结果无效"
	case strings.Contains(detail, "分组用量统计失败"):
		return "统计失败，等待重试"
	case window == "新增请求":
		return "新增请求"
	case window == "账号倍率变更":
		return "账号倍率变更"
	case window == "无新增，冻结":
		return "无新增，保持不变"
	case window == "待发布重试":
		return "待发布重试"
	case strings.HasPrefix(window, "初始化"):
		return "初始化"
	case window != "":
		return window
	case detail == "":
		return ""
	case strings.Contains(detail, "F=") || strings.Contains(detail, "M=") ||
		strings.Contains(detail, "P=") || strings.Contains(detail, "目标="):
		return ""
	default:
		return compactReportExplanation(detail)
	}
}

func compactReportExplanation(detail string) string {
	if index := strings.IndexAny(detail, "；;"); index >= 0 {
		detail = detail[:index]
	}
	runes := []rune(strings.TrimSpace(detail))
	const maxRunes = 18
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "..."
	}
	return string(runes)
}

func formatTableRow(cells []string, widths []int) string {
	parts := make([]string, len(cells))
	for index, cell := range cells {
		padding := widths[index] - displayWidth(cell)
		parts[index] = cell + strings.Repeat(" ", padding)
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
			// 组合附加符号不占用额外的终端显示单元。
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
