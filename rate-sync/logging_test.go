package main

import (
	"strings"
	"testing"
)

func TestClassifyLogLine(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		status logStatus
	}{
		{name: "success update", line: "2026/08/26 09:21:14 已更新账号 foo(1) 上游倍率", status: logStatusOK},
		{name: "failure", line: "2026/08/26 09:21:14 [foo] 同步失败: 请求上游失败", status: logStatusFail},
		{name: "skip", line: "2026/08/26 09:21:14 [foo] 暂不自动: 多账号", status: logStatusSkip},
		{name: "summary failure", line: "2026/08/26 09:21:14 同步检查完成: 可用绑定=1 已检查=1 检查正常=0 暂不自动=0 失败=1", status: logStatusFail},
		{name: "summary skip", line: "2026/08/26 09:21:14 同步检查完成: 可用绑定=2 已检查=1 检查正常=1 暂不自动=1 失败=0", status: logStatusSkip},
		{name: "summary success", line: "2026/08/26 09:21:14 同步检查完成: 可用绑定=1 已检查=1 检查正常=1 暂不自动=0 失败=0", status: logStatusOK},
		{name: "run", line: "2026/08/26 09:21:13 开始自动发现并同步价格", status: logStatusRun},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyLogLine(tt.line); got != tt.status {
				t.Fatalf("classifyLogLine() = %q, want %q", got, tt.status)
			}
		})
	}
}

func TestStatusLogWriterAddsMarkerAfterTimestamp(t *testing.T) {
	var output strings.Builder
	writer := &statusLogWriter{dst: &output}
	if _, err := writer.Write([]byte("2026/08/26 09:21:14 [foo] 已更新账号 foo(1) 上游倍率\n")); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "2026/08/26 09:21:14 [OK] [foo] 已更新账号") {
		t.Fatalf("missing status marker: %q", got)
	}
}

func TestStatusLogWriterCanColorOnlyTheMarker(t *testing.T) {
	var output strings.Builder
	writer := &statusLogWriter{dst: &output, color: true}
	if _, err := writer.Write([]byte("2026/08/26 09:21:14 [foo] 同步失败: 请求上游失败\n")); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, ansiRed+"[FAIL] "+ansiReset) {
		t.Fatalf("missing red failure marker: %q", got)
	}
	if strings.Contains(got, ansiRed+"[FAIL] "+ansiReset+"2026") {
		t.Fatal("color should not wrap the whole line")
	}
}

func TestStatusLogWriterDoesNotDuplicateMarker(t *testing.T) {
	var output strings.Builder
	writer := &statusLogWriter{dst: &output}
	line := "2026/08/26 09:21:14 [FAIL] [foo] 同步失败: 请求上游失败\n"
	if _, err := writer.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != line {
		t.Fatalf("marker duplicated or line changed: %q", got)
	}
}

func TestStatusLogWriterDoesNotDecorateTableMarker(t *testing.T) {
	var output strings.Builder
	writer := &statusLogWriter{dst: &output}
	line := "2026/08/26 09:21:14 [TABLE] 账号 | 分组 | 账户倍率\n"
	if _, err := writer.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != line {
		t.Fatalf("table marker duplicated or line changed: %q", got)
	}
}
