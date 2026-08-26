package main

import (
	"strings"
	"testing"
)

func TestSyncReportRendersUpdatedGroupRatesWithoutAccountFields(t *testing.T) {
	channel := testChannel("https://upstream.example", 0.1)
	channel.AccountRateMultiplier = 0.1
	channel.AccountName = "账号 A"
	channel.Group.Name = "分组 A"
	channel.ProxyURL = "http://user:secret@proxy.example:7897"
	report := newSyncReport("group", []Channel{channel})
	report.updateGroupRate(channel.Group.ID, 0.1234)
	report.markGroup(channel.Group.ID, reportStatusUpdated)

	output := strings.Join(report.tableLines(), "\n")
	if !strings.Contains(output, "分组 A") || !strings.Contains(output, "0.1234") {
		t.Fatalf("missing updated rate row: %s", output)
	}
	if !strings.Contains(output, "已更新") {
		t.Fatalf("missing status: %s", output)
	}
	if strings.Contains(output, "账号 A") || strings.Contains(output, "账号倍率") || strings.Contains(output, "代理") {
		t.Fatalf("group table should not contain account or proxy fields: %s", output)
	}
	if strings.Contains(output, " | ") {
		t.Fatalf("table should use fixed-width spaces instead of pipe separators: %s", output)
	}
	if strings.Contains(output, "proxy.example") || strings.Contains(output, "secret") || strings.Contains(output, "user@") {
		t.Fatalf("group table should not render proxy details: %s", output)
	}
}

func TestSyncReportAccountTableRendersSanitizedProxy(t *testing.T) {
	channel := testChannel("https://upstream.example", 0.1)
	channel.AccountName = "账号 A"
	channel.ProxyURL = "http://user:secret@proxy.example:7897"
	report := newSyncReport("account", []Channel{channel})
	report.markAccount(channel.AccountID, reportStatusStable)

	output := strings.Join(report.tableLines(), "\n")
	if !strings.Contains(output, "代理") || !strings.Contains(output, "http://proxy.example:7897") {
		t.Fatalf("account table should render sanitized proxy: %s", output)
	}
	if strings.Contains(output, "secret") || strings.Contains(output, "user@") {
		t.Fatalf("proxy credentials leaked: %s", output)
	}
}

func TestSyncReportAlignsWideCharacters(t *testing.T) {
	first := testChannel("https://upstream.example", 0.1)
	first.AccountName = "刀哥"
	first.Group.Name = "分组 A"
	second := testChannel("https://upstream.example", 0.1)
	second.AccountName = "long-account"
	second.Group.ID++
	second.Group.Name = "long-group"
	report := newSyncReport("group", []Channel{first, second})
	report.markGroup(first.Group.ID, reportStatusStable)
	report.markGroup(second.Group.ID, reportStatusStable)

	lines := report.tableLines()
	if len(lines) != 4 {
		t.Fatalf("expected header, separator, and two rows; got %d lines", len(lines))
	}
	headerRate := tableColumnDisplayStart(lines[0], "分组倍率")
	firstRate := tableColumnDisplayStart(lines[2], "0.1000")
	if headerRate < 0 || firstRate != headerRate {
		t.Fatalf("group-rate column is not aligned: header=%d row=%d\n%s", headerRate, firstRate, strings.Join(lines, "\n"))
	}
	if tableColumnDisplayStart(lines[0], "结果") != tableColumnDisplayStart(lines[2], "稳定") {
		t.Fatalf("result column is not aligned:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(lines[0], "代理") {
		t.Fatalf("group table should not contain a proxy column:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSyncReportGroupSummaryKeepsEvidenceBelowTable(t *testing.T) {
	first := testChannel("https://upstream.example", 0.1)
	first.AccountName = "账号甲"
	first.AccountRateMultiplier = 0.25
	first.Group.Name = "共享分组"
	first.ProxyURL = "http://user:secret@proxy.example:7897"
	second := first
	second.AccountID++
	second.AccountName = "账号乙"
	second.AccountRateMultiplier = 0.5

	report := newSyncReport("group", []Channel{first, second, second})
	report.setGroupEvidence(first.Group.ID, "过去 30 天", strings.Repeat("很长的说明", 30))
	lines := report.tableLines()
	output := strings.Join(lines, "\n")

	if len(lines) != 4 {
		t.Fatalf("expected one group row and one evidence line; got %d lines: %s", len(lines), output)
	}
	if strings.Contains(output, "账号甲") || strings.Contains(output, "账号乙") || strings.Contains(output, "账号倍率") {
		t.Fatalf("group table mixed in account information: %s", output)
	}
	if strings.Contains(lines[2], "很长的说明") || !strings.HasPrefix(lines[3], "  ↳ 分组 共享分组：") {
		t.Fatalf("evidence should be a clearly marked indented line: %s", output)
	}
	if displayWidth(lines[2]) >= displayWidth(lines[3]) {
		t.Fatalf("long evidence should not widen the table row: %s", output)
	}
	if strings.Contains(output, "代理") || strings.Contains(output, "proxy.example") || strings.Contains(output, "secret") || strings.Contains(output, "user@") {
		t.Fatalf("group table should not render proxy details: %s", output)
	}
}

func TestSyncReportGroupTableDoesNotMixAccountRates(t *testing.T) {
	first := testChannel("https://upstream.example", 0.1)
	first.AccountName = "lucen-gpt-006"
	first.AccountRateMultiplier = 0.051
	second := first
	second.AccountID++
	second.AccountName = "lucen-gpt-008"
	second.AccountRateMultiplier = 0.068
	report := newSyncReport("group", []Channel{first, second})
	lines := report.tableLines()
	output := strings.Join(lines, "\n")
	if strings.Contains(output, "006=0.0510") || strings.Contains(output, "008=0.0680") {
		t.Fatalf("group table should not render account rates: %s", output)
	}
	if strings.Contains(output, "账号倍率") {
		t.Fatalf("group table header should not contain account-rate column: %s", output)
	}
}

func TestSyncReportGroupTableKeepsResultAfterRate(t *testing.T) {
	channel := testChannel("https://upstream.example", 0.1)
	report := newSyncReport("group", []Channel{channel})
	report.markGroup(channel.Group.ID, reportStatusStable)
	lines := report.tableLines()
	if tableColumnDisplayStart(lines[0], "结果") != tableColumnDisplayStart(lines[2], "稳定") {
		t.Fatalf("result column is not aligned:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(lines[0], "代理") {
		t.Fatalf("group table should not contain a proxy column:\n%s", lines[0])
	}
}

func TestSyncReportAccountSummaryDeduplicatesGroups(t *testing.T) {
	first := testChannel("https://upstream.example", 0.1)
	first.AccountName = "账号甲"
	first.AccountRateMultiplier = 0.75
	first.Group.Name = "分组甲"
	second := first
	second.Group.ID++
	second.Group.Name = "分组乙"
	second.Group.RateMultiplier = 0.2

	report := newSyncReport("account", []Channel{first, second, second})
	lines := report.tableLines()
	output := strings.Join(lines, "\n")

	if len(lines) != 3 {
		t.Fatalf("expected one account row; got %d lines: %s", len(lines), output)
	}
	if strings.Count(output, "账号甲") != 1 || !strings.Contains(lines[2], "0.7500") {
		t.Fatalf("account row did not render account information: %s", output)
	}
	if strings.Contains(output, "分组倍率") || strings.Contains(output, "分组甲") || strings.Contains(output, "分组乙") {
		t.Fatalf("account table mixed in group information: %s", output)
	}
}

func tableColumnDisplayStart(line, value string) int {
	index := strings.Index(line, value)
	if index < 0 {
		return -1
	}
	return displayWidth(line[:index])
}
