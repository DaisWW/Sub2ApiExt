package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
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
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum-1]) + "…"
}

func tableAccountLabel(name string, id int64, maximum int) string {
	label := accountLabel(name, id)
	if len([]rune(label)) <= maximum {
		return label
	}
	suffix := fmt.Sprintf(" #%d", id)
	namePart := strings.TrimSuffix(label, suffix)
	nameMaximum := maximum - len([]rune(suffix))
	if nameMaximum < 2 {
		return suffix
	}
	return truncateTableLabel(namePart, nameMaximum) + suffix
}

// writeRecommendationTable 输出一份适合 docker logs 直接阅读的摘要表。
// 账户名统一使用 accountLabel，JSON 报告仍保留原始 name 字段供程序消费。
func writeRecommendationTable(writer io.Writer, generatedAt string, recommendations []Recommendation) error {
	if writer == nil {
		return nil
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintf(table, "\n优先级账户表（%s）\n", generatedAt); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(table, "ACCOUNT\tSCORE\tPRIORITY\tCURRENT\tSAMPLES\tAVAIL\tCOST/M\tP90(ms)\tACTION"); err != nil {
		return err
	}
	for _, recommendation := range recommendations {
		samples := recommendation.SuccessfulRequests + recommendation.TerminalFailures
		cost := "-"
		if recommendation.CostPerMillionTokens > 0 {
			cost = fmt.Sprintf("%.4f", recommendation.CostPerMillionTokens)
		}
		p90 := "-"
		if recommendation.LatencyP90Ms > 0 {
			p90 = fmt.Sprintf("%.0f", recommendation.LatencyP90Ms)
		} else if recommendation.FirstTokenP90Ms > 0 {
			p90 = fmt.Sprintf("%.0f*", recommendation.FirstTokenP90Ms)
		}
		action := recommendation.ApplyStatus
		if action == "" {
			action = "unchanged"
		}
		if _, err := fmt.Fprintf(table, "%s\t%.1f\t%d\t%d\t%d\t%.1f%%\t%s\t%s\t%s\n",
			tableAccountLabel(recommendation.Name, recommendation.ID, 36),
			recommendation.Score,
			recommendation.RecommendedPriority,
			recommendation.CurrentPriority,
			samples,
			recommendation.Availability*100,
			cost,
			p90,
			action,
		); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(table); err != nil {
		return err
	}
	return table.Flush()
}
