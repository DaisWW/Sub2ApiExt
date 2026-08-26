package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store { return &Store{db: db} }

const cycleAdvisoryLockID int64 = 734205318

func (s *Store) AcquireCycleLease(ctx context.Context) (release func(), acquired bool, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, cycleAdvisoryLockID).Scan(&locked); err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	if !locked {
		_ = conn.Close()
		return nil, false, nil
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, cycleAdvisoryLockID)
		_ = conn.Close()
	}, true, nil
}

func (s *Store) EnsureSchema(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS monitoring_targets (
    target_key TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    entity_id BIGINT NOT NULL,
    name TEXT NOT NULL,
    platform TEXT NOT NULL DEFAULT '',
    source_status TEXT NOT NULL DEFAULT '',
    probe_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS monitoring_checks (
    id BIGSERIAL PRIMARY KEY,
    target_key TEXT NOT NULL,
    kind TEXT NOT NULL,
    entity_id BIGINT NOT NULL,
    group_id BIGINT,
    status TEXT NOT NULL,
    latency_ms INTEGER,
    first_byte_ms INTEGER,
    status_code INTEGER,
    error_class TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS monitoring_checks_target_time_idx
    ON monitoring_checks (target_key, checked_at DESC);
CREATE INDEX IF NOT EXISTS monitoring_checks_time_idx
    ON monitoring_checks (checked_at);
CREATE TABLE IF NOT EXISTS monitoring_alert_states (
    target_key TEXT PRIMARY KEY,
    observed_status TEXT NOT NULL,
    failure_streak INTEGER NOT NULL DEFAULT 0,
    recovery_streak INTEGER NOT NULL DEFAULT 0,
    alerted_status TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS monitoring_alerts (
    id BIGSERIAL PRIMARY KEY,
    target_key TEXT NOT NULL,
    target_name TEXT NOT NULL,
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    title TEXT NOT NULL,
    message TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    acknowledged_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS monitoring_alerts_created_idx
    ON monitoring_alerts (created_at DESC);
`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

// LoadSnapshot reads current account/group membership in one query. It is
// deliberately read-only: probe failures must never mutate gateway routing.
func (s *Store) LoadSnapshot(ctx context.Context) (model.Snapshot, error) {
	const query = `
SELECT a.id, a.name, a.platform, a.type, a.status, a.schedulable, a.credentials,
       a.proxy_id, p.protocol, p.host, p.port, p.username, p.password, p.status,
       recent.created_at, recent.model,
       g.id, g.name, g.platform, g.status
FROM accounts a
LEFT JOIN proxies p ON p.id = a.proxy_id AND p.deleted_at IS NULL
LEFT JOIN LATERAL (
    SELECT ul.created_at, ul.model
    FROM usage_logs ul
    WHERE ul.account_id = a.id
    ORDER BY ul.created_at DESC, ul.id DESC
    LIMIT 1
) recent ON TRUE
LEFT JOIN account_groups ag ON ag.account_id = a.id
LEFT JOIN groups g ON g.id = ag.group_id AND g.deleted_at IS NULL
WHERE a.deleted_at IS NULL
ORDER BY a.id, g.id`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return model.Snapshot{}, fmt.Errorf("load account snapshot: %w", err)
	}
	defer rows.Close()

	accounts := make(map[int64]*model.Account)
	groups := make(map[int64]*model.Group)
	for rows.Next() {
		var (
			id, groupID, proxyID                  sql.NullInt64
			name, platform, accountType, status   sql.NullString
			proxyProtocol, proxyHost, proxyUser   sql.NullString
			proxyPassword, proxyStatus            sql.NullString
			proxyPort                             sql.NullInt64
			lastActivity                          sql.NullTime
			recentModel                           sql.NullString
			schedulable                           bool
			credentials                           []byte
			groupName, groupPlatform, groupStatus sql.NullString
		)
		if err := rows.Scan(&id, &name, &platform, &accountType, &status, &schedulable, &credentials,
			&proxyID, &proxyProtocol, &proxyHost, &proxyPort, &proxyUser, &proxyPassword, &proxyStatus, &lastActivity, &recentModel,
			&groupID, &groupName, &groupPlatform, &groupStatus); err != nil {
			return model.Snapshot{}, fmt.Errorf("scan account snapshot: %w", err)
		}
		if !id.Valid {
			continue
		}
		account := accounts[id.Int64]
		if account == nil {
			account = &model.Account{
				ID: id.Int64, Name: name.String, Platform: platform.String,
				Type: accountType.String, Status: status.String,
				Schedulable: schedulable, Credentials: map[string]any{},
			}
			if len(credentials) > 0 {
				if err := json.Unmarshal(credentials, &account.Credentials); err != nil {
					return model.Snapshot{}, fmt.Errorf("decode credentials for account %d: %w", id.Int64, err)
				}
			}
			if lastActivity.Valid {
				value := lastActivity.Time.UTC()
				account.LastActivityAt = &value
			}
			if recentModel.Valid {
				account.RecentModel = strings.TrimSpace(recentModel.String)
			}
			if proxyID.Valid {
				if !proxyProtocol.Valid || !proxyHost.Valid || !proxyPort.Valid || proxyStatus.String != "active" {
					account.ProxyError = "configured proxy is unavailable"
				} else {
					account.ProxyURL = buildProxyURL(proxyProtocol.String, proxyHost.String, int(proxyPort.Int64), proxyUser.String, proxyPassword.String)
				}
			}
			accounts[id.Int64] = account
		}
		if !groupID.Valid {
			continue
		}
		if !containsID(account.GroupIDs, groupID.Int64) {
			account.GroupIDs = append(account.GroupIDs, groupID.Int64)
		}
		group := groups[groupID.Int64]
		if group == nil {
			group = &model.Group{ID: groupID.Int64, Name: groupName.String, Platform: groupPlatform.String, Status: groupStatus.String}
			groups[groupID.Int64] = group
		}
		if !containsID(group.AccountIDs, account.ID) {
			group.AccountIDs = append(group.AccountIDs, account.ID)
		}
	}
	if err := rows.Err(); err != nil {
		return model.Snapshot{}, err
	}

	// Keep an explicit ungrouped bucket so accounts without a binding are not
	// silently omitted from the grouped view.
	ungrouped := &model.Group{ID: -1, Name: "Ungrouped", Platform: "mixed", Status: "active"}
	for _, account := range accounts {
		if len(account.GroupIDs) == 0 {
			ungrouped.AccountIDs = append(ungrouped.AccountIDs, account.ID)
		}
	}
	if len(ungrouped.AccountIDs) > 0 {
		groups[ungrouped.ID] = ungrouped
	}
	for _, group := range groups {
		if !groupIsActive(group.Status) {
			group.ProbeEnabled = false
			continue
		}
		for _, accountID := range group.AccountIDs {
			if account, ok := accounts[accountID]; ok && accountProbeEligible(*account) {
				group.ProbeEnabled = true
				break
			}
		}
	}

	snapshot := model.Snapshot{
		Accounts: make([]model.Account, 0, len(accounts)),
		Groups:   make([]model.Group, 0, len(groups)),
	}
	for _, account := range accounts {
		snapshot.Accounts = append(snapshot.Accounts, *account)
	}
	for _, group := range groups {
		snapshot.Groups = append(snapshot.Groups, *group)
	}
	if err := s.syncTargets(ctx, snapshot); err != nil {
		return model.Snapshot{}, err
	}
	return snapshot, nil
}

func (s *Store) syncTargets(ctx context.Context, snapshot model.Snapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE monitoring_targets SET active = FALSE, updated_at = NOW()`); err != nil {
		return err
	}
	const upsert = `
INSERT INTO monitoring_targets (target_key, kind, entity_id, name, platform, source_status, probe_enabled, active, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE,NOW())
ON CONFLICT (target_key) DO UPDATE SET
    kind = EXCLUDED.kind, entity_id = EXCLUDED.entity_id, name = EXCLUDED.name,
    platform = EXCLUDED.platform, source_status = EXCLUDED.source_status,
    probe_enabled = EXCLUDED.probe_enabled, active = TRUE, updated_at = NOW()`
	for _, account := range snapshot.Accounts {
		probeEnabled := accountProbeEligible(account)
		if _, err := tx.ExecContext(ctx, upsert, model.TargetKey(model.KindAccount, account.ID), model.KindAccount,
			account.ID, account.Name, account.Platform, account.Status, probeEnabled); err != nil {
			return fmt.Errorf("upsert account target %d: %w", account.ID, err)
		}
	}
	for _, group := range snapshot.Groups {
		if _, err := tx.ExecContext(ctx, upsert, model.TargetKey(model.KindGroup, group.ID), model.KindGroup,
			group.ID, group.Name, group.Platform, group.Status, group.ProbeEnabled); err != nil {
			return fmt.Errorf("upsert group target %d: %w", group.ID, err)
		}
	}
	return tx.Commit()
}

func (s *Store) InsertResults(ctx context.Context, results []model.ProbeResult) error {
	if len(results) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const query = `INSERT INTO monitoring_checks
    (target_key, kind, entity_id, group_id, status, latency_ms, first_byte_ms, status_code, error_class, message, checked_at)
    VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
	for _, result := range results {
		if _, err := tx.ExecContext(ctx, query, result.TargetKey, result.Kind, result.EntityID, result.GroupID,
			result.Status, result.LatencyMs, result.FirstByteMs, result.StatusCode, result.ErrorClass, result.Message, result.CheckedAt); err != nil {
			return fmt.Errorf("insert monitoring result: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) Prune(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM monitoring_checks WHERE checked_at < $1`, before)
	return err
}

func (s *Store) Dashboard(ctx context.Context, windowDays int, staleAfter time.Duration, intervalSec int) (model.Dashboard, error) {
	const query = `
WITH samples AS (
    SELECT target_key, status, latency_ms, first_byte_ms, checked_at, 'probe'::text AS source
    FROM monitoring_checks
    WHERE checked_at >= NOW() - ($1::int * INTERVAL '1 day')
    UNION ALL
    SELECT 'account:' || account_id::text, 'operational', duration_ms, first_token_ms, created_at, 'history'
    FROM usage_logs
    WHERE created_at >= NOW() - ($1::int * INTERVAL '1 day')
    UNION ALL
    SELECT 'group:' || COALESCE(group_id, -1)::text, 'operational', duration_ms, first_token_ms, created_at, 'history'
    FROM usage_logs
    WHERE created_at >= NOW() - ($1::int * INTERVAL '1 day')
), latest AS (
    SELECT DISTINCT ON (target_key) target_key, status, latency_ms, first_byte_ms, checked_at, source
    FROM samples
    ORDER BY target_key, checked_at DESC, CASE WHEN source = 'history' THEN 0 ELSE 1 END
), stats AS (
    SELECT target_key,
           COUNT(*) FILTER (WHERE status NOT IN ('unknown','disabled')) AS samples,
           COUNT(*) FILTER (WHERE status IN ('operational','degraded')) AS successful,
           MIN(first_byte_ms) FILTER (WHERE status IN ('operational','degraded')) AS first_fastest,
           percentile_cont(0.5) WITHIN GROUP (ORDER BY first_byte_ms) FILTER (WHERE status IN ('operational','degraded') AND first_byte_ms IS NOT NULL) AS first_median,
           MAX(first_byte_ms) FILTER (WHERE status IN ('operational','degraded')) AS first_slowest,
           MIN(latency_ms) FILTER (WHERE status IN ('operational','degraded')) AS latency_fastest,
           percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE status IN ('operational','degraded') AND latency_ms IS NOT NULL) AS latency_median,
           MAX(latency_ms) FILTER (WHERE status IN ('operational','degraded')) AS latency_slowest
    FROM samples
    GROUP BY target_key
)
SELECT t.target_key, t.kind, t.entity_id, t.name, t.platform, t.source_status, t.probe_enabled,
       l.status, l.latency_ms, l.first_byte_ms, l.checked_at, l.source,
       COALESCE(s.samples,0), COALESCE(s.successful,0),
       s.first_fastest, s.first_median, s.first_slowest,
       s.latency_fastest, s.latency_median, s.latency_slowest
FROM monitoring_targets t
LEFT JOIN latest l ON l.target_key = t.target_key
LEFT JOIN stats s ON s.target_key = t.target_key
WHERE t.active = TRUE
ORDER BY CASE WHEN t.kind = 'group' THEN 0 ELSE 1 END, t.name, t.entity_id`
	rows, err := s.db.QueryContext(ctx, query, windowDays)
	if err != nil {
		return model.Dashboard{}, fmt.Errorf("load dashboard: %w", err)
	}
	defer rows.Close()
	now := time.Now().UTC()
	dashboard := model.Dashboard{GeneratedAt: now, WindowDays: windowDays, IntervalSec: intervalSec, Targets: []model.DashboardTarget{}}
	availabilityTargets := 0
	for rows.Next() {
		var target model.DashboardTarget
		var (
			latestStatus, latestSource                                 sql.NullString
			latestLatency, latestFirst                                 sql.NullInt64
			latestAt                                                   sql.NullTime
			samples, successful                                        int
			firstFastest, firstSlowest, latencyFastest, latencySlowest sql.NullInt64
			firstMedian, latencyMedian                                 sql.NullFloat64
		)
		if err := rows.Scan(&target.Key, &target.Kind, &target.EntityID, &target.Name, &target.Platform, &target.SourceStatus,
			&target.ProbeEnabled, &latestStatus, &latestLatency, &latestFirst, &latestAt, &latestSource, &samples, &successful,
			&firstFastest, &firstMedian, &firstSlowest, &latencyFastest, &latencyMedian, &latencySlowest); err != nil {
			return model.Dashboard{}, fmt.Errorf("scan dashboard: %w", err)
		}
		target.Status = model.StatusUnknown
		if !target.ProbeEnabled {
			target.Status = model.StatusDisabled
		} else if latestStatus.Valid {
			target.Status = latestStatus.String
		}
		if latestAt.Valid {
			at := latestAt.Time.UTC()
			target.LastCheckedAt = &at
			target.Stale = now.Sub(at) > staleAfter
		}
		if latestSource.Valid {
			target.LatestSource = latestSource.String
		}
		if latestLatency.Valid {
			value := int(latestLatency.Int64)
			target.LatestLatencyMs = &value
		}
		if latestFirst.Valid {
			value := int(latestFirst.Int64)
			target.LatestFirstByteMs = &value
		}
		stats := model.TargetStats{Samples: samples, Successful: successful, Errors: samples - successful}
		if samples > 0 {
			stats.Availability = float64(successful) * 100 / float64(samples)
			if target.ProbeEnabled {
				availabilityTargets++
			}
		}
		stats.FirstByte = metricStats(firstFastest, firstMedian, firstSlowest)
		stats.Latency = metricStats(latencyFastest, latencyMedian, latencySlowest)
		target.Stats = stats
		dashboard.Targets = append(dashboard.Targets, target)
		dashboard.Summary.Targets++
		switch target.Status {
		case model.StatusOperational:
			dashboard.Summary.Operational++
		case model.StatusDegraded:
			dashboard.Summary.Degraded++
		case model.StatusFailed, model.StatusError:
			dashboard.Summary.Failed++
		default:
			dashboard.Summary.Unknown++
		}
		if target.ProbeEnabled {
			dashboard.Summary.Availability += stats.Availability
		}
	}
	if err := rows.Err(); err != nil {
		return model.Dashboard{}, err
	}
	if availabilityTargets > 0 {
		dashboard.Summary.Availability /= float64(availabilityTargets)
	}
	return dashboard, nil
}

func metricStats(fastest sql.NullInt64, median sql.NullFloat64, slowest sql.NullInt64) model.MetricStats {
	var out model.MetricStats
	if fastest.Valid {
		value := int(fastest.Int64)
		out.FastestMs = &value
	}
	if median.Valid {
		value := median.Float64
		out.MedianMs = &value
	}
	if slowest.Valid {
		value := int(slowest.Int64)
		out.SlowestMs = &value
	}
	return out
}

func (s *Store) History(ctx context.Context, key string, days, limit int) ([]model.ProbeResult, error) {
	if limit <= 0 || limit > 1000 {
		limit = 240
	}
	if days <= 0 || days > 90 {
		days = 7
	}
	const query = `
SELECT target_key, kind, entity_id, group_id, status, latency_ms, first_byte_ms,
       status_code, error_class, message, checked_at, source
FROM (
    SELECT target_key, kind, entity_id, group_id, status, latency_ms, first_byte_ms,
           status_code, error_class, message, checked_at, 'probe'::text AS source
    FROM monitoring_checks
    WHERE target_key = $1 AND checked_at >= NOW() - ($2::int * INTERVAL '1 day')
    UNION ALL
    SELECT 'account:' || account_id::text, 'account', account_id, group_id, 'operational',
           duration_ms, first_token_ms, NULL::integer, '', 'real request history', created_at, 'history'
    FROM usage_logs
    WHERE $1 = 'account:' || account_id::text
      AND created_at >= NOW() - ($2::int * INTERVAL '1 day')
    UNION ALL
    SELECT 'group:' || COALESCE(group_id, -1)::text, 'group', COALESCE(group_id, -1), group_id, 'operational',
           duration_ms, first_token_ms, NULL::integer, '', 'real request history', created_at, 'history'
    FROM usage_logs
    WHERE $1 = 'group:' || COALESCE(group_id, -1)::text
      AND created_at >= NOW() - ($2::int * INTERVAL '1 day')
) combined
ORDER BY checked_at DESC
LIMIT $3`
	rows, err := s.db.QueryContext(ctx, query, key, days, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.ProbeResult, 0)
	for rows.Next() {
		var result model.ProbeResult
		var groupID, latency, firstByte, statusCode sql.NullInt64
		if err := rows.Scan(&result.TargetKey, &result.Kind, &result.EntityID, &groupID, &result.Status,
			&latency, &firstByte, &statusCode, &result.ErrorClass, &result.Message, &result.CheckedAt, &result.Source); err != nil {
			return nil, err
		}
		if groupID.Valid {
			value := groupID.Int64
			result.GroupID = &value
		}
		if latency.Valid {
			value := int(latency.Int64)
			result.LatencyMs = &value
		}
		if firstByte.Valid {
			value := int(firstByte.Int64)
			result.FirstByteMs = &value
		}
		if statusCode.Valid {
			value := int(statusCode.Int64)
			result.StatusCode = &value
		}
		out = append(out, result)
	}
	return out, rows.Err()
}

func (s *Store) Alerts(ctx context.Context, onlyUnacknowledged bool, limit int) ([]model.Alert, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id, target_key, target_name, kind, status, title, message, created_at, acknowledged_at
FROM monitoring_alerts`
	args := []any{}
	if onlyUnacknowledged {
		query += ` WHERE acknowledged_at IS NULL`
	}
	query += ` ORDER BY created_at DESC LIMIT $1`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Alert, 0)
	for rows.Next() {
		var alert model.Alert
		if err := rows.Scan(&alert.ID, &alert.TargetKey, &alert.TargetName, &alert.Kind, &alert.Status, &alert.Title,
			&alert.Message, &alert.CreatedAt, &alert.AcknowledgedAt); err != nil {
			return nil, err
		}
		out = append(out, alert)
	}
	return out, rows.Err()
}

func (s *Store) AcknowledgeAlert(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE monitoring_alerts SET acknowledged_at = COALESCE(acknowledged_at, NOW()) WHERE id = $1`, id)
	return err
}

// EvaluateAlert is the only place that turns transient probe results into a
// user-facing notification. Consecutive streaks prevent noisy flapping alerts.
func (s *Store) EvaluateAlert(ctx context.Context, result model.ProbeResult, name string, policy model.AlertPolicy) error {
	if result.Status != model.StatusOperational && result.Status != model.StatusDegraded && result.Status != model.StatusFailed && result.Status != model.StatusError {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var observed string
	var failureStreak, recoveryStreak int
	var alertedStatus string
	err = tx.QueryRowContext(ctx, `SELECT observed_status, failure_streak, recovery_streak, alerted_status
FROM monitoring_alert_states WHERE target_key = $1 FOR UPDATE`, result.TargetKey).
		Scan(&observed, &failureStreak, &recoveryStreak, &alertedStatus)
	if err == sql.ErrNoRows {
		observed = result.Status
	} else if err != nil {
		return err
	}
	isFailure := result.Status == model.StatusFailed || result.Status == model.StatusError
	var eventStatus string
	if isFailure {
		failureStreak++
		recoveryStreak = 0
		if failureStreak >= policy.FailureThreshold && alertedStatus != model.StatusFailed {
			eventStatus = model.StatusFailed
			alertedStatus = model.StatusFailed
		}
	} else {
		failureStreak = 0
		recoveryStreak++
		if recoveryStreak >= policy.RecoveryThreshold && alertedStatus == model.StatusFailed {
			eventStatus = model.StatusOperational
			alertedStatus = model.StatusOperational
		}
	}
	observed = result.Status
	if err == sql.ErrNoRows {
		_, err = tx.ExecContext(ctx, `INSERT INTO monitoring_alert_states
 (target_key, observed_status, failure_streak, recovery_streak, alerted_status)
 VALUES ($1,$2,$3,$4,$5)`, result.TargetKey, observed, failureStreak, recoveryStreak, alertedStatus)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE monitoring_alert_states SET observed_status=$2, failure_streak=$3,
 recovery_streak=$4, alerted_status=$5, updated_at=NOW() WHERE target_key=$1`, result.TargetKey, observed, failureStreak, recoveryStreak, alertedStatus)
	}
	if err != nil {
		return err
	}
	if eventStatus != "" {
		title, message := alertText(name, result, eventStatus)
		_, err = tx.ExecContext(ctx, `INSERT INTO monitoring_alerts
 (target_key, target_name, kind, status, title, message) VALUES ($1,$2,$3,$4,$5,$6)`,
			result.TargetKey, name, result.Kind, eventStatus, title, message)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func alertText(name string, result model.ProbeResult, status string) (string, string) {
	if status == model.StatusOperational {
		return "服务已恢复", fmt.Sprintf("%s 的主动探测已恢复响应。", name)
	}
	message := result.Message
	if strings.TrimSpace(message) == "" {
		message = "主动探测连续失败"
	}
	return "服务不可用", fmt.Sprintf("%s 不可用：%s", name, message)
}

func containsID(values []int64, value int64) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func accountProbeEligible(account model.Account) bool {
	return account.Status == "error" || (account.Status == "active" && account.Schedulable)
}

func groupIsActive(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "active")
}

func buildProxyURL(protocol, host string, port int, username, password string) string {
	proxyURL := &url.URL{
		Scheme: strings.TrimSpace(protocol),
		Host:   net.JoinHostPort(strings.TrimSpace(host), strconv.Itoa(port)),
	}
	if username != "" && password != "" {
		proxyURL.User = url.UserPassword(username, password)
	}
	return proxyURL.String()
}
