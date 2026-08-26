package main

import (
	"fmt"
	"strings"
	"unicode"
)

type reportTableRow struct {
	cells    []string
	evidence []string
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
	lines := make([]string, 0, len(rows)*2+2)
	lines = append(lines, formatTableRow(headers, widths))
	lines = append(lines, formatTableSeparator(widths))
	for _, row := range rows {
		lines = append(lines, formatTableRow(row.cells, widths))
		lines = append(lines, row.evidence...)
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
		evidence string
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
		if summary.evidence == "" {
			summary.evidence = reportEvidence(row)
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
			},
			evidence: reportEvidenceLines(summary.name, summary.evidence),
		})
	}
	return []string{"分组", "分组倍率", "结果"}, rows
}

func (r *syncReport) accountSummaryRows(keys []string) ([]string, []reportTableRow) {
	type accountSummary struct {
		name     string
		rate     float64
		proxies  []string
		statuses []string
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
			summary = &accountSummary{name: row.accountName, rate: row.accountRate}
			summaries[row.accountID] = summary
			order = append(order, row.accountID)
		}
		summary.proxies = appendUnique(summary.proxies, row.proxy)
		summary.statuses = appendUnique(summary.statuses, row.status)
	}

	rows := make([]reportTableRow, 0, len(order))
	for _, accountID := range order {
		summary := summaries[accountID]
		rows = append(rows, reportTableRow{
			cells: []string{
				tableCell(summary.name),
				fmt.Sprintf("%.4f", summary.rate),
				tableCell(strings.Join(summary.statuses, ", ")),
				tableCell(strings.Join(summary.proxies, ", ")),
			},
		})
	}
	return []string{"账号", "账户倍率", "结果", "代理"}, rows
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func reportEvidence(row *syncReportRow) string {
	evidence := row.window
	if row.detail == "" {
		return evidence
	}
	if evidence == "" {
		return row.detail
	}
	return evidence + "；" + row.detail
}

func reportEvidenceLines(groupName, evidence string) []string {
	if evidence == "" {
		return nil
	}
	return []string{fmt.Sprintf("  ↳ 分组 %s：%s", tableCell(groupName), tableCell(evidence))}
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
