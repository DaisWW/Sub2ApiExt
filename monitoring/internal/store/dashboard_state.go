package store

import (
	"database/sql"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

// applyLatestTargetState keeps the original helper signature for callers that
// do not need to surface a group-health explanation.
func applyLatestTargetState(
	target *model.DashboardTarget,
	status, source sql.NullString,
	latency, firstByte sql.NullInt64,
	checkedAt sql.NullTime,
	now time.Time,
	staleAfter time.Duration,
) {
	applyLatestTargetStateWithMessage(target, status, source, sql.NullString{}, latency, firstByte, checkedAt, now, staleAfter)
}

func applyLatestTargetStateWithMessage(
	target *model.DashboardTarget,
	status, source sql.NullString,
	message sql.NullString,
	latency, firstByte sql.NullInt64,
	checkedAt sql.NullTime,
	now time.Time,
	staleAfter time.Duration,
) {
	target.Status = model.StatusUnknown
	target.Available = false
	// Groups are never probed directly. Their aggregate check is still valid
	// evidence, so a disabled group probe must not erase that health state.
	// A disabled account, on the other hand, is not a routable target.
	if !target.ProbeEnabled && target.Kind != model.KindGroup {
		target.Status = model.StatusDisabled
		return
	}
	if status.Valid {
		target.Status = status.String
		target.Available = target.Status == model.StatusOperational || target.Status == model.StatusDegraded
		if target.Status == model.StatusUnknown {
			if target.Kind == model.KindGroup {
				target.LatestMessage = "暂无账户健康证据，等待真实请求或恢复探测"
			} else if strings.EqualFold(strings.TrimSpace(target.SourceStatus), "error") {
				if target.RecoveryTriggerAt != nil {
					target.LatestMessage = "渠道报错，等待恢复探测"
				} else {
					target.LatestMessage = "账户处于错误状态；等待真实请求或新的渠道错误"
				}
			} else {
				target.LatestMessage = "暂无真实请求，等待渠道错误或下一次请求确认"
			}
		}
	} else {
		if target.Kind == model.KindGroup {
			target.LatestMessage = "暂无账户健康证据，等待真实请求或恢复探测"
		} else if strings.EqualFold(strings.TrimSpace(target.SourceStatus), "error") {
			if target.RecoveryTriggerAt != nil {
				target.LatestMessage = "渠道报错，等待恢复探测"
			} else {
				target.LatestMessage = "账户处于错误状态；等待真实请求或新的渠道错误"
			}
		} else {
			target.LatestMessage = "暂无真实请求；仅在渠道报错后主动探测"
		}
	}
	if checkedAt.Valid {
		value := checkedAt.Time.UTC()
		target.LastCheckedAt = &value
		target.Stale = now.Sub(value) > staleAfter
	}
	if source.Valid {
		target.LatestSource = source.String
	}
	if message.Valid && (target.Kind == model.KindGroup || (source.Valid && source.String == "request_error")) {
		if sanitized := sanitizeUpstreamMessage(message.String); sanitized != "" {
			target.LatestMessage = sanitized
		}
	}
	if latency.Valid {
		value := int(latency.Int64)
		target.LatestLatencyMs = &value
	}
	if firstByte.Valid {
		value := int(firstByte.Int64)
		target.LatestFirstByteMs = &value
	}
}
