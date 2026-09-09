package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// accountLabel 是日志和报告表格使用的稳定账户标识。名称可能包含换行或
// 控制字符，先压成单行，避免污染容器日志；没有名称时仍保留稳定 ID。
func accountLabel(name string, id int64) string {
	cleaned := strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return ' '
		}
		return value
	}, name)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if cleaned == "" {
		return "#" + strconv.FormatInt(id, 10)
	}
	return cleaned + " #" + strconv.FormatInt(id, 10)
}

func truncateTableLabel(value string, maximum int) string {
	if maximum < 2 {
		return ""
	}
	if displayWidth(value) <= maximum {
		return value
	}
	remaining := maximum - displayWidth("…")
	var result strings.Builder
	width := 0
	for _, character := range value {
		runeWidth := runeDisplayWidth(character)
		if width+runeWidth > remaining {
			break
		}
		result.WriteRune(character)
		width += runeWidth
	}
	return result.String() + "…"
}

func tableAccountLabel(name string, id int64, maximum int) string {
	label := accountLabel(name, id)
	if displayWidth(label) <= maximum {
		return label
	}
	suffix := fmt.Sprintf(" #%d", id)
	namePart := strings.TrimSuffix(label, suffix)
	nameMaximum := maximum - displayWidth(suffix)
	if nameMaximum < 2 {
		return suffix
	}
	return truncateTableLabel(namePart, nameMaximum) + suffix
}

func runeDisplayWidth(value rune) int {
	if unicode.IsControl(value) || unicode.Is(unicode.Mn, value) || unicode.Is(unicode.Me, value) {
		return 0
	}
	if (value >= 0x1100 && value <= 0x115f) ||
		(value >= 0x2329 && value <= 0x232a) ||
		(value >= 0x2e80 && value <= 0xa4cf) ||
		(value >= 0xac00 && value <= 0xd7a3) ||
		(value >= 0xf900 && value <= 0xfaff) ||
		(value >= 0xfe10 && value <= 0xfe6f) ||
		(value >= 0xff00 && value <= 0xff60) ||
		(value >= 0xffe0 && value <= 0xffe6) ||
		(value >= 0x1f300 && value <= 0x1faff) {
		return 2
	}
	return 1
}

func displayWidth(value string) int {
	width := 0
	for _, character := range value {
		width += runeDisplayWidth(character)
	}
	return width
}

func tableColumnWidths(rows [][]string) []int {
	if len(rows) == 0 {
		return nil
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for index, value := range row {
			if index < len(widths) && displayWidth(value) > widths[index] {
				widths[index] = displayWidth(value)
			}
		}
	}
	return widths
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

func formatTableRows(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	widths := tableColumnWidths(rows)
	var output strings.Builder
	output.WriteString(formatTableRow(rows[0], widths))
	output.WriteByte('\n')
	output.WriteString(formatTableSeparator(widths))
	output.WriteByte('\n')
	for _, row := range rows[1:] {
		output.WriteString(formatTableRow(row, widths))
		output.WriteByte('\n')
	}
	return output.String()
}

func tableActionLabel(value string) string {
	switch value {
	case "", "unchanged":
		return "保持"
	case "updated":
		return "已更新"
	case "pending":
		return "待确认"
	case "cooldown":
		return "冷却中"
	case "exploring":
		return "探索中"
	case "exploration-started":
		return "开始探索"
	case "exploration-ended":
		return "探索结束"
	case "deferred-exploration":
		return "等待探索"
	case "dry-run":
		return "试运行"
	case "missing-key":
		return "缺少密钥"
	case "failed":
		return "写入失败"
	default:
		return value
	}
}

func formatTableTime(value string) string {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return parsed.Local().Format("2006-01-02 15:04:05")
}

func formatTableLatency(milliseconds float64, fallback bool) string {
	if milliseconds <= 0 {
		return "-"
	}
	value := ""
	if milliseconds >= 1000 {
		value = fmt.Sprintf("%.1fs", milliseconds/1000)
	} else {
		value = fmt.Sprintf("%.0fms", milliseconds)
	}
	if fallback {
		value += "*"
	}
	return value
}

// writeRecommendationTable 输出一份适合 docker logs 直接阅读的摘要表。
// 账户名统一使用 accountLabel，JSON 报告仍保留原始 name 字段供程序消费。
func writeRecommendationTable(writer io.Writer, generatedAt string, recommendations []Recommendation) error {
	if writer == nil {
		return nil
	}
	rows := [][]string{{"账号", "分数", "优先级", "当前", "样本", "可用", "成本/M", "延迟P90", "状态"}}
	for _, recommendation := range recommendations {
		samples := recommendation.SuccessfulRequests + recommendation.TerminalFailures
		cost := "-"
		if recommendation.CostPerMillionTokens > 0 {
			cost = fmt.Sprintf("%.4f", recommendation.CostPerMillionTokens)
		}
		p90 := "-"
		if recommendation.LatencyP90Ms > 0 {
			p90 = formatTableLatency(recommendation.LatencyP90Ms, false)
		} else if recommendation.FirstTokenP90Ms > 0 {
			p90 = formatTableLatency(recommendation.FirstTokenP90Ms, true)
		}
		action := tableActionLabel(recommendation.ApplyStatus)
		rows = append(rows, []string{
			tableAccountLabel(recommendation.Name, recommendation.ID, 32),
			fmt.Sprintf("%.1f", recommendation.Score),
			strconv.Itoa(recommendation.RecommendedPriority),
			strconv.Itoa(recommendation.CurrentPriority),
			strconv.FormatInt(samples, 10),
			fmt.Sprintf("%.1f%%", recommendation.Availability*100),
			cost,
			p90,
			action,
		})
	}
	var output strings.Builder
	fmt.Fprintf(&output, "优先级账户表（%s；共 %d 个）\n", formatTableTime(generatedAt), len(recommendations))
	output.WriteString(formatTableRows(rows))
	output.WriteByte('\n')
	_, err := io.WriteString(writer, output.String())
	return err
}
