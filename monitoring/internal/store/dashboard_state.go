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
	sourceError := strings.EqualFold(strings.TrimSpace(target.SourceStatus), "error")
	insufficientEvidence := status.Valid &&
		strings.EqualFold(strings.TrimSpace(status.String), model.StatusUnknown) && !sourceError
	// No health evidence is an idle/insufficient-data state. Keep enabled
	// targets usable by default; explicit gateway errors still remain visible
	// as a diagnostic state below.
	target.Status = model.StatusOperational
	target.Available = true
	// Groups are never probed directly. Their aggregate check is still valid
	// evidence, so a disabled group probe must not erase that health state.
	// A disabled account, on the other hand, is not a routable target.
	if !target.ProbeEnabled && target.Kind != model.KindGroup {
		target.Status = model.StatusDisabled
		target.Available = false
		return
	}
	if status.Valid {
		target.Status = status.String
		target.Available = target.Status == model.StatusOperational || target.Status == model.StatusDegraded
		if target.Status == model.StatusUnknown {
			if sourceError {
				if target.RecoveryTriggerAt != nil {
					target.LatestMessage = "渠道报错，等待恢复探测"
				} else {
					target.LatestMessage = "账户处于错误状态；等待真实请求或新的渠道错误"
				}
			} else {
				// Unknown checks are lack of evidence rather than a failed route.
				target.Status = model.StatusOperational
				target.Available = true
				target.LatestMessage = ""
			}
		}
	} else {
		if sourceError {
			if target.RecoveryTriggerAt != nil {
				target.Status = model.StatusFailed
				target.Available = false
				target.LatestMessage = "渠道报错，等待恢复探测"
			} else {
				target.Status = model.StatusUnknown
				target.Available = false
				target.LatestMessage = "账户处于错误状态；等待真实请求或新的渠道错误"
			}
		} else {
			target.LatestMessage = ""
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
	if insufficientEvidence {
		// A persisted unknown aggregate may still carry its old "无法确认" text.
		// Unknown without an explicit channel error is insufficient evidence, so
		// expose the usable default instead of retaining a failure-sounding note.
		target.LatestMessage = "数据不足，默认按可用处理"
	} else if message.Valid && (target.Kind == model.KindGroup || (source.Valid && source.String == "request_error")) {
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
