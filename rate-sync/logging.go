package main

import (
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
)

type logStatus string

const (
	logStatusInfo logStatus = "INFO"
	logStatusRun  logStatus = "RUN"
	logStatusOK   logStatus = "OK"
	logStatusSkip logStatus = "SKIP"
	logStatusFail logStatus = "FAIL"
)

const (
	ansiReset  = "\x1b[0m"
	ansiCyan   = "\x1b[36;1m"
	ansiGreen  = "\x1b[32;1m"
	ansiRed    = "\x1b[31;1m"
	ansiYellow = "\x1b[33;1m"
)

// statusLogWriter 在标准日志时间戳后加入简短、稳定的状态标记。
// 标记始终输出；ANSI 颜色可选，因为 Docker 日志收集器可能不会渲染转义序列。
type statusLogWriter struct {
	dst   io.Writer
	color bool
	mu    sync.Mutex
}

func newStatusLogger(dst io.Writer) *log.Logger {
	return log.New(&statusLogWriter{dst: dst, color: logColorEnabled()}, "", log.LstdFlags)
}

func (w *statusLogWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	var output strings.Builder
	for start := 0; start < len(p); {
		end := strings.IndexByte(string(p[start:]), '\n')
		if end < 0 {
			end = len(p) - start
		} else {
			end++
		}
		line := string(p[start : start+end])
		output.WriteString(w.decorateLine(line))
		start += end
	}

	if _, err := io.WriteString(w.dst, output.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *statusLogWriter) decorateLine(line string) string {
	lineBody := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if lineBody == "" {
		return line
	}
	if hasStatusMarker(lineBody) {
		return line
	}

	status := classifyLogLine(lineBody)
	marker := "[" + string(status) + "] "
	if w.color {
		marker = statusColor(status) + marker + ansiReset
	}

	insertAt := logTimestampEnd(lineBody)
	decorated := lineBody[:insertAt] + marker + lineBody[insertAt:]
	if strings.HasSuffix(line, "\r\n") {
		return decorated + "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return decorated + "\n"
	}
	return decorated
}

func classifyLogLine(line string) logStatus {
	message := line[logTimestampEnd(line):]

	if strings.Contains(message, "同步检查完成:") {
		if summaryCount(message, "失败") > 0 {
			return logStatusFail
		}
		if summaryCount(message, "暂不自动") > 0 {
			return logStatusSkip
		}
		return logStatusOK
	}

	if containsAny(message,
		"启动失败",
		"同步周期失败",
		"同步失败:",
		"请求失败:",
		"更新失败",
		"失败:",
		"错误:",
	) {
		return logStatusFail
	}
	if containsAny(message,
		"暂不自动:",
		"校准跳过:",
		"本轮跳过",
		"保持当前",
		"等待账户倍率同步",
		"动态成本初始化等待",
		"动态成本跳过",
	) {
		return logStatusSkip
	}
	if containsAny(message,
		"单账号直接使用账户倍率",
		"已按单账号倍率更新分组",
		"已更新账号",
		"已更新分组",
		"已按历史成本更新分组",
		"历史成本校准稳定",
		"动态成本冻结",
		"动态成本稳定",
		"已按动态成本更新分组",
		"本地倍率与本轮检测一致",
		"检查成功:",
	) {
		return logStatusOK
	}
	if containsAny(message, "已启动", "开始自动发现") {
		return logStatusRun
	}
	return logStatusInfo
}

func summaryCount(message, label string) int {
	marker := label + "="
	index := strings.Index(message, marker)
	if index < 0 {
		return 0
	}
	index += len(marker)
	start := index
	for index < len(message) && message[index] >= '0' && message[index] <= '9' {
		index++
	}
	if start == index {
		return 0
	}
	value, err := strconv.Atoi(message[start:index])
	if err != nil {
		return 0
	}
	return value
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func hasStatusMarker(line string) bool {
	rest := line[logTimestampEnd(line):]
	if strings.HasPrefix(rest, "[TABLE] ") {
		return true
	}
	for _, status := range []logStatus{logStatusInfo, logStatusRun, logStatusOK, logStatusSkip, logStatusFail} {
		if strings.HasPrefix(rest, "["+string(status)+"] ") {
			return true
		}
	}
	return false
}

func logTimestampEnd(line string) int {
	// log.LstdFlags 格式为“2009/01/23 01:23:23”并紧跟一个空格。
	if len(line) >= 20 &&
		line[4] == '/' && line[7] == '/' && line[10] == ' ' &&
		line[13] == ':' && line[16] == ':' && line[19] == ' ' {
		return 20
	}
	return 0
}

func statusColor(status logStatus) string {
	switch status {
	case logStatusRun:
		return ansiCyan
	case logStatusOK:
		return ansiGreen
	case logStatusSkip:
		return ansiYellow
	case logStatusFail:
		return ansiRed
	default:
		return "\x1b[36m"
	}
}

func logColorEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RATE_SYNC_LOG_COLOR"))) {
	case "always", "true", "1", "yes":
		return true
	case "never", "false", "0", "no":
		return false
	}

	// 自动模式只在交互式终端启用颜色；日志查看器明确支持 ANSI 时可设为 always。
	if os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	info, err := os.Stderr.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
