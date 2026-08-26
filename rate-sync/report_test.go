package main

import (
	"strings"
	"testing"
)

func TestSyncReportRendersUpdatedRatesAndProxyWithoutCredentials(t *testing.T) {
	channel := testChannel("https://upstream.example", 0.1)
	channel.AccountRateMultiplier = 0.1
	channel.AccountName = "账号 A"
	channel.Group.Name = "分组 A"
	channel.ProxyURL = "http://user:secret@proxy.example:7897"
	report := newSyncReport("group", []Channel{channel})
	report.updateGroupRate(channel.Group.ID, 0.1234)
	report.markGroup(channel.Group.ID, reportStatusUpdated)

	output := strings.Join(report.tableLines(), "\n")
	if !strings.Contains(output, "账号 A  分组 A") || !strings.Contains(output, "0.1000") || !strings.Contains(output, "0.1234") {
		t.Fatalf("missing updated rate row: %s", output)
	}
	if !strings.Contains(output, "http://proxy.example:7897") || !strings.Contains(output, "已更新") {
		t.Fatalf("missing sanitized proxy/status: %s", output)
	}
	if strings.Contains(output, " | ") {
		t.Fatalf("table should use fixed-width spaces instead of pipe separators: %s", output)
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

	lines := report.tableLines()
	if len(lines) != 4 {
		t.Fatalf("expected header, separator, and two rows; got %d lines", len(lines))
	}
	headerGroup := tableColumnDisplayStart(lines[0], "分组")
	firstGroup := tableColumnDisplayStart(lines[2], "分组 A")
	if headerGroup < 0 || firstGroup != headerGroup {
		t.Fatalf("group column is not aligned: header=%d row=%d\n%s", headerGroup, firstGroup, strings.Join(lines, "\n"))
	}
	if tableColumnDisplayStart(lines[0], "代理") != tableColumnDisplayStart(lines[2], "直连") {
		t.Fatalf("proxy column is not aligned:\n%s", strings.Join(lines, "\n"))
	}
}

func tableColumnDisplayStart(line, value string) int {
	index := strings.Index(line, value)
	if index < 0 {
		return -1
	}
	return displayWidth(line[:index])
}
