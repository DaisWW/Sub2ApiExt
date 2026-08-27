package store

import (
	"strings"
	"testing"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

func TestAlertStateAppliesFailureAndRecoveryThresholds(t *testing.T) {
	policy := model.AlertPolicy{FailureThreshold: 2, RecoveryThreshold: 1}
	var state alertState
	if event := state.observe(model.StatusError, policy); event != "" {
		t.Fatalf("首次失败产生了告警：%q", event)
	}
	if event := state.observe(model.StatusFailed, policy); event != model.StatusFailed {
		t.Fatalf("连续失败事件为 %q，期望 %q", event, model.StatusFailed)
	}
	if event := state.observe(model.StatusOperational, policy); event != model.StatusOperational {
		t.Fatalf("恢复事件为 %q，期望 %q", event, model.StatusOperational)
	}
}

func TestAlertStateDoesNotTreatDegradedAsRecovery(t *testing.T) {
	policy := model.AlertPolicy{FailureThreshold: 1, RecoveryThreshold: 2}
	var state alertState
	if event := state.observe(model.StatusFailed, policy); event != model.StatusFailed {
		t.Fatalf("failure event = %q, want %q", event, model.StatusFailed)
	}
	if event := state.observe(model.StatusDegraded, policy); event != "" {
		t.Fatalf("degraded observation produced recovery event %q", event)
	}
	if state.recoveryStreak != 0 || state.alertedStatus != model.StatusFailed {
		t.Fatalf("degraded observation changed recovery state: %+v", state)
	}
	if event := state.observe(model.StatusOperational, policy); event != "" {
		t.Fatalf("first operational observation produced recovery event %q", event)
	}
	if event := state.observe(model.StatusOperational, policy); event != model.StatusOperational {
		t.Fatalf("clean recovery event = %q, want %q", event, model.StatusOperational)
	}
}

func TestAlertTextUsesObservationSource(t *testing.T) {
	_, message := alertText("账户 A", model.ProbeResult{Source: "history"}, model.StatusOperational)
	if !strings.Contains(message, "真实请求") {
		t.Fatalf("恢复文案未说明真实请求来源：%q", message)
	}
}

func TestSanitizeUpstreamMessageRemovesLegacyBody(t *testing.T) {
	got := sanitizeUpstreamMessage("账户 A 不可用：HTTP 502: internal secret")
	if got != "账户 A 不可用：HTTP 502" {
		t.Fatalf("脱敏结果为 %q", got)
	}
	if got := sanitizeUpstreamMessage(`账户 A 不可用：Post "https://example.com/v1/chat/completions": context deadline exceeded`); got != "账户 A 不可用：上游请求超时" {
		t.Fatalf("超时错误未归一化为 %q", got)
	}
	if got := sanitizeUpstreamMessage("upstream request failed"); got != "上游请求失败" {
		t.Fatalf("英文错误未归一化为 %q", got)
	}
	if got := sanitizeUpstreamMessage("分组 A 不可用：0/3 accounts healthy"); got != "分组 A 不可用：分组内健康账户 0/3" {
		t.Fatalf("分组错误未归一化为 %q", got)
	}
	if got := sanitizeUpstreamMessage("当前路由可用：1/2 accounts healthy；异常 1，待验证 0；预计失败暴露 37.0%"); got != "当前仍可用：1/2 个候选；异常 1，待验证 0；预计失败暴露 37.0%" {
		t.Fatalf("分组可用性文案未保留路由语义：%q", got)
	}
}
