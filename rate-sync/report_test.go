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

func TestSyncReportAccountTableShowsRateSource(t *testing.T) {
	upstream := testChannel("https://upstream.example", 0.1)
	upstream.AccountName = "上游账号"
	upstream.AccountRateMultiplier = 0.1
	calculated := upstream
	calculated.AccountID++
	calculated.Group.ID++
	calculated.AccountName = "计算账号"
	report := newSyncReport("account", []Channel{upstream, calculated})
	report.setAccountSource(upstream.AccountID, reportAccountSourceUpstream)
	report.setAccountSource(calculated.AccountID, reportAccountSourceUsage)
	report.setAccountUpstreamRate(upstream.AccountID, 0.1234)
	report.setAccountUpstreamRate(calculated.AccountID, 0.2345)
	report.setAccountRechargeDiscount(upstream.AccountID, 0.9)
	report.setAccountRechargeDiscount(calculated.AccountID, 0.95)
	report.setAccountExpectedRate(upstream.AccountID, 0.1111)
	report.setAccountExpectedRate(calculated.AccountID, 0.2228)
	report.markAccount(upstream.AccountID, reportStatusStable)
	report.markAccount(calculated.AccountID, reportStatusUpdated)
	report.updateAccountRate(calculated.AccountID, 0.18)

	output := strings.Join(report.tableLines(), "\n")
	if !strings.Contains(output, "稳定｜上游同步") || !strings.Contains(output, "已更新｜请求计算｜原 0.1000") {
		t.Fatalf("account table should show the rate source in the result: %s", output)
	}
	for _, header := range []string{"当前倍率", "上游倍率", "充值折扣", "预期倍率"} {
		if !strings.Contains(output, header) {
			t.Fatalf("account table should show the %s column: %s", header, output)
		}
	}
	if !strings.Contains(output, "0.2228") || !strings.Contains(output, "0.1234") || !strings.Contains(output, "0.2345") {
		t.Fatalf("account table should show upstream and expected rates: %s", output)
	}
	if !strings.Contains(output, "充值折扣") || !strings.Contains(output, "0.9000") {
		t.Fatalf("account table should show the configured recharge discount: %s", output)
	}
	lines := strings.Split(output, "\n")
	resultStart := tableColumnDisplayStart(lines[0], "结果")
	discountStart := tableColumnDisplayStart(lines[0], "充值折扣")
	upstreamStart := tableColumnDisplayStart(lines[0], "上游倍率")
	expectedStart := tableColumnDisplayStart(lines[0], "预期倍率")
	proxyStart := tableColumnDisplayStart(lines[0], "代理")
	if upstreamStart < 0 || discountStart < 0 || expectedStart < 0 {
		t.Fatalf("account rate columns are missing: %s", output)
	}
	for _, check := range []struct {
		value string
		start int
	}{
		{value: "0.1234", start: upstreamStart},
		{value: "0.2345", start: upstreamStart},
		{value: "0.9000", start: discountStart},
		{value: "0.9500", start: discountStart},
		{value: "0.1111", start: expectedStart},
		{value: "0.2228", start: expectedStart},
	} {
		var row string
		for _, line := range lines[2:] {
			if strings.Contains(line, check.value) {
				row = line
				break
			}
		}
		if tableColumnDisplayStart(row, check.value) != check.start {
			t.Fatalf("account value %q is not aligned:\n%s", check.value, output)
		}
	}
	for _, source := range []string{"上游同步", "请求计算"} {
		var row string
		for _, line := range lines[2:] {
			if strings.Contains(line, source) {
				row = line
				break
			}
		}
		sourceStart := tableColumnDisplayStart(row, source)
		if resultStart < 0 || proxyStart <= resultStart || sourceStart < resultStart || sourceStart >= proxyStart {
			t.Fatalf("rate source %q is outside the result column:\n%s", source, output)
		}
	}
}

func TestSyncReportAccountTableShowsDashWithoutCandidate(t *testing.T) {
	channel := testChannel("https://upstream.example", 0.1)
	report := newSyncReport("account", []Channel{channel})
	report.markAccount(channel.AccountID, reportStatusChecked)

	lines := report.tableLines()
	if len(lines) != 3 {
		t.Fatalf("expected one account row: %s", strings.Join(lines, "\n"))
	}
	if strings.Count(lines[2], "-") < 2 {
		t.Fatalf("missing dash for unavailable upstream and expected rates: %s", lines[2])
	}
}

func TestSyncReportAccountTableDistinguishesRateSources(t *testing.T) {
	probe := testChannel("https://probe.example", 0.1)
	probe.AccountName = "自动探测账号"
	probe.AccountID = 31
	probe.Group.ID = 31
	upstream := testChannel("https://newapi.example", 0.1)
	upstream.AccountName = "上游同步账号"
	upstream.AccountID = 32
	upstream.Group.ID = 32
	calculated := testChannel("https://usage.example", 0.1)
	calculated.AccountName = "请求计算账号"
	calculated.AccountID = 33
	calculated.Group.ID = 33

	report := newSyncReport("account", []Channel{probe, upstream, calculated})
	report.setAccountSource(probe.AccountID, reportAccountSourceProbe)
	report.setAccountSource(upstream.AccountID, reportAccountSourceUpstream)
	report.setAccountSource(calculated.AccountID, reportAccountSourceUsage)
	report.markAccount(probe.AccountID, reportStatusStable)
	report.markAccount(upstream.AccountID, reportStatusStable)
	report.markAccount(calculated.AccountID, reportStatusStable)

	output := strings.Join(report.tableLines(), "\n")
	for _, source := range []string{"稳定｜自动探测", "稳定｜上游同步", "稳定｜请求计算"} {
		if !strings.Contains(output, source) {
			t.Fatalf("missing rate source %q: %s", source, output)
		}
	}
}

func TestSyncReportAccountTableShowsExpectedRateBeforeCandidate(t *testing.T) {
	channel := testChannel("https://upstream.example", 0.1)
	channel.AccountName = "待同步账号"
	channel.AccountRateMultiplier = 0.2
	report := newSyncReport("account", []Channel{channel})
	report.setAccountSource(channel.AccountID, reportAccountSourceUsage)
	report.setAccountExpectedRate(channel.AccountID, 0.09)
	report.markAccount(channel.AccountID, reportStatusChecked)

	lines := report.tableLines()
	if len(lines) != 3 || !strings.Contains(lines[2], "0.2000") || !strings.Contains(lines[2], "0.0900") ||
		!strings.Contains(lines[2], "检查｜请求计算") {
		t.Fatalf("expected current, expected, and checked values in one row:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(lines[2], "原 0.2000") {
		t.Fatalf("checked row should not show an update-only previous-rate marker:\n%s", strings.Join(lines, "\n"))
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

func TestSyncReportGroupSummaryRendersExplanationColumn(t *testing.T) {
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

	if len(lines) != 3 {
		t.Fatalf("expected header, separator, and one group row; got %d lines: %s", len(lines), output)
	}
	if !strings.Contains(lines[0], "说明") {
		t.Fatalf("group table should include a dedicated explanation column: %s", output)
	}
	if strings.Contains(output, "账号甲") || strings.Contains(output, "账号乙") || strings.Contains(output, "账号倍率") {
		t.Fatalf("group table mixed in account information: %s", output)
	}
	if !strings.Contains(lines[2], "过去 30 天") || strings.Contains(lines[2], "很长的说明") {
		t.Fatalf("group explanation should stay concise and on the group row: %s", output)
	}
	if strings.Contains(output, "↳") {
		t.Fatalf("group explanation should not be rendered as a separate line: %s", output)
	}
	if strings.Contains(output, "代理") || strings.Contains(output, "proxy.example") || strings.Contains(output, "secret") || strings.Contains(output, "user@") {
		t.Fatalf("group table should not render proxy details: %s", output)
	}
}

func TestSyncReportGroupTableKeepsExplanationsWithRows(t *testing.T) {
	first := testChannel("https://upstream.example", 0.1)
	first.Group.Name = "分组甲"
	second := first
	second.Group.ID++
	second.Group.Name = "分组乙"
	report := newSyncReport("group", []Channel{first, second})
	report.setGroupEvidence(first.Group.ID, "窗口甲", "说明甲")
	report.setGroupEvidence(second.Group.ID, "窗口乙", "说明乙")

	lines := report.tableLines()
	if len(lines) != 4 {
		t.Fatalf("expected header, separator, and two group rows; got %d lines: %s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "说明") {
		t.Fatalf("group table should include a dedicated explanation column: %s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[2], "分组甲") || !strings.Contains(lines[2], "窗口甲") {
		t.Fatalf("first group explanation should stay on its row: %s", strings.Join(lines, "\n"))
	}
	if strings.Contains(lines[2], "窗口乙") || strings.Contains(lines[2], "说明乙") {
		t.Fatalf("first group row mixed in the second group's explanation: %s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[3], "分组乙") || !strings.Contains(lines[3], "窗口乙") || strings.Contains(lines[3], "窗口甲") {
		t.Fatalf("second group explanation should stay on its row: %s", strings.Join(lines, "\n"))
	}
	if tableColumnDisplayStart(lines[0], "说明") != tableColumnDisplayStart(lines[2], "窗口甲") ||
		tableColumnDisplayStart(lines[0], "说明") != tableColumnDisplayStart(lines[3], "窗口乙") {
		t.Fatalf("explanation column is not aligned:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(strings.Join(lines, "\n"), "↳") {
		t.Fatalf("explanations should not be rendered as separate lines: %s", strings.Join(lines, "\n"))
	}
}

func TestSyncReportGroupSummarySimplifiesDynamicEvidence(t *testing.T) {
	channel := testChannel("https://upstream.example", 0.1)
	report := newSyncReport("group", []Channel{channel})
	report.setGroupEvidence(channel.Group.ID, "新增请求", "新增=$1.0505 F=0.1999 M=0.1854 P=0.1999 目标=0.1988")

	output := strings.Join(report.tableLines(), "\n")
	if !strings.Contains(output, "新增请求") {
		t.Fatalf("group table should keep the short explanation: %s", output)
	}
	for _, detail := range []string{"F=", "M=", "P=", "目标="} {
		if strings.Contains(output, detail) {
			t.Fatalf("group table should not expose dynamic calculation detail %q: %s", detail, output)
		}
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
