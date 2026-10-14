package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const responseLimit = 16 * 1024 * 1024

func readUpstreamAnswer(resp *http.Response, protocol string, r *Result) {
	body := bufio.NewReader(resp.Body)
	stream := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	if !stream && protocol != "gemini" {
		// Some Codex/compatible upstreams omit the SSE content type.
		prefix, _ := body.Peek(64)
		start := strings.TrimSpace(strings.TrimPrefix(string(prefix), "\ufeff"))
		stream = strings.HasPrefix(start, "data:") || strings.HasPrefix(start, "event:") || strings.HasPrefix(start, ":")
	}
	readAnswer(body, stream, protocol, r)
}

func readAnswer(body io.Reader, stream bool, protocol string, r *Result) {
	r.Status = "failed"
	limited := &io.LimitedReader{R: body, N: responseLimit + 1}
	if !stream {
		b, err := io.ReadAll(limited)
		if len(b) > responseLimit {
			r.Error = "响应超过 16MB"
			return
		}
		if err != nil {
			r.Error = "响应读取失败"
			return
		}
		var v map[string]any
		if json.Unmarshal(b, &v) != nil {
			r.Error = "上游未返回有效 JSON"
			return
		}
		readJSON(v, protocol, r)
	} else {
		var answer strings.Builder
		scanner := bufio.NewScanner(limited)
		scanner.Buffer(make([]byte, 8192), responseLimit)
		data := []string{}
		consume := func() bool {
			if len(data) == 0 {
				return false
			}
			raw := strings.Join(data, "\n")
			data = nil
			if raw == "[DONE]" {
				return true
			}
			var v map[string]any
			if json.Unmarshal([]byte(raw), &v) != nil {
				r.Status = "failed"
				r.Error = "无效的流式事件"
				return true
			}
			if v["error"] != nil || v["type"] == "error" || v["type"] == "response.failed" {
				r.Status = "failed"
				r.Error = "上游在流中返回错误"
				return true
			}
			switch protocol {
			case "responses":
				switch str(v["type"]) {
				case "response.output_text.delta":
					answer.WriteString(str(v["delta"]))
				case "response.completed", "response.done":
					response := obj(v["response"])
					if answer.Len() == 0 {
						answer.WriteString(responseText(response))
					}
					usage(obj(response["usage"]), r)
					if status := str(response["status"]); status != "" && status != "completed" {
						r.Status = "incomplete"
						r.Error = "上游未完整生成回答"
					} else {
						r.Status = "completed"
					}
					return true
				case "response.incomplete":
					r.Status = "incomplete"
					r.Error = "输出被截断"
					return true
				}
			case "chat":
				usage(obj(v["usage"]), r)
				for _, choice := range arr(v["choices"]) {
					c := obj(choice)
					answer.WriteString(str(obj(c["delta"])["content"]))
					if reason := str(c["finish_reason"]); reason != "" {
						if reason == "stop" {
							r.Status = "completed"
						} else {
							r.Status = "incomplete"
							r.Error = "输出结束原因：" + safeReason(reason)
						}
					}
				}
			case "anthropic":
				switch str(v["type"]) {
				case "message_start":
					usage(obj(obj(v["message"])["usage"]), r)
				case "content_block_delta":
					if d := obj(v["delta"]); d["type"] == "text_delta" {
						answer.WriteString(str(d["text"]))
					}
				case "message_delta":
					usage(obj(v["usage"]), r)
					reason := str(obj(v["delta"])["stop_reason"])
					if reason == "end_turn" || reason == "stop_sequence" {
						r.Status = "completed"
					} else {
						r.Status = "incomplete"
						r.Error = "输出结束原因：" + safeReason(reason)
					}
				case "message_stop":
					return true
				}
			}
			return false
		}
		terminal := false
		for scanner.Scan() {
			line := strings.TrimSuffix(scanner.Text(), "\r")
			if line == "" {
				if consume() {
					terminal = true
					break
				}
			} else if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if !terminal && len(data) > 0 {
			terminal = consume()
		}
		if limited.N <= 0 {
			r.Status = "incomplete"
			r.Error = "响应超过 16MB"
		} else if scanner.Err() != nil {
			r.Status = "incomplete"
			r.Error = "流读取失败"
		} else if !terminal {
			r.Status = "incomplete"
			r.Error = "连接在完成事件之前断开"
		}
		r.Answer = answer.String()
	}
	if r.Status == "completed" && strings.TrimSpace(r.Answer) == "" {
		r.Status = "failed"
		r.Error = "没有收到文本回答"
	}
	if r.Status == "failed" && r.Error == "" {
		r.Error = "缺少有效完成事件"
	}
}

func readJSON(v map[string]any, protocol string, r *Result) {
	if v["error"] != nil {
		r.Error = "上游返回错误"
		return
	}
	switch protocol {
	case "responses":
		r.Answer = responseText(v)
		usage(obj(v["usage"]), r)
		if v["status"] == "completed" {
			r.Status = "completed"
		} else {
			r.Status = "incomplete"
			r.Error = "上游未完整生成回答"
		}
	case "chat":
		usage(obj(v["usage"]), r)
		for _, item := range arr(v["choices"]) {
			c := obj(item)
			r.Answer += str(obj(c["message"])["content"])
			if c["finish_reason"] == "stop" {
				r.Status = "completed"
			} else {
				r.Status = "incomplete"
				r.Error = "输出被截断或包含工具调用"
			}
		}
	case "anthropic":
		for _, item := range arr(v["content"]) {
			c := obj(item)
			if c["type"] == "text" {
				r.Answer += str(c["text"])
			}
		}
		usage(obj(v["usage"]), r)
		if v["stop_reason"] == "end_turn" || v["stop_reason"] == "stop_sequence" {
			r.Status = "completed"
		} else {
			r.Status = "incomplete"
			r.Error = "输出未完整结束"
		}
	case "gemini":
		for _, item := range arr(v["candidates"]) {
			c := obj(item)
			for _, p := range arr(obj(c["content"])["parts"]) {
				part := obj(p)
				if part["thought"] != true {
					r.Answer += str(part["text"])
				}
			}
			if c["finishReason"] == "STOP" {
				r.Status = "completed"
			} else {
				r.Status = "incomplete"
				r.Error = "输出被截断或拦截"
			}
		}
		u := obj(v["usageMetadata"])
		r.InputTokens = number(u["promptTokenCount"])
		r.OutTokens = number(u["candidatesTokenCount"])
	}
}

func str(v any) string         { s, _ := v.(string); return s }
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func arr(v any) []any          { a, _ := v.([]any); return a }
func number(v any) int64       { n, _ := v.(float64); return int64(n) }
func usage(u map[string]any, r *Result) {
	if v, ok := u["input_tokens"]; ok {
		r.InputTokens = number(v)
	}
	if v, ok := u["output_tokens"]; ok {
		r.OutTokens = number(v)
	}
	if v, ok := u["prompt_tokens"]; ok {
		r.InputTokens = number(v)
	}
	if v, ok := u["completion_tokens"]; ok {
		r.OutTokens = number(v)
	}
}
func responseText(v map[string]any) string {
	var b strings.Builder
	for _, o := range arr(v["output"]) {
		for _, c := range arr(obj(o)["content"]) {
			m := obj(c)
			if m["type"] == "output_text" {
				b.WriteString(str(m["text"]))
			}
		}
	}
	return b.String()
}
func safeReason(s string) string {
	switch s {
	case "length", "max_tokens", "tool_calls", "content_filter", "tool_use", "pause_turn":
		return s
	default:
		return "未正常结束"
	}
}

var htmlFence = regexp.MustCompile("(?is)```(?:html|svg)\\s*\\n(.*?)```")
var htmlStart = regexp.MustCompile(`(?i)<!doctype\s+html|<html[\s>]|<svg[\s>]`)

func extractHTML(answer string) string {
	html := strings.TrimSpace(answer)
	if m := htmlFence.FindStringSubmatch(html); len(m) > 1 {
		html = strings.TrimSpace(m[1])
	} else if pos := htmlStart.FindStringIndex(html); pos != nil {
		html = html[pos[0]:]
	} else {
		return ""
	}
	lower := strings.ToLower(html)
	for _, end := range []string{"</html>", "</svg>"} {
		if i := strings.LastIndex(lower, end); i >= 0 {
			html = html[:i+len(end)]
			break
		}
	}
	if strings.HasPrefix(strings.ToLower(html), "<svg") {
		html = "<!doctype html><html><body>" + html + "</body></html>"
	}
	return html
}
