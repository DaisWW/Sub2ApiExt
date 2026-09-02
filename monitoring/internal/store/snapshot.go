package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

const snapshotQuery = `
WITH recent_account_usage AS MATERIALIZED (
    SELECT DISTINCT ON (ul.account_id) ul.account_id, ul.created_at, ul.model
    FROM usage_logs ul
    JOIN (
        SELECT account_id, MAX(created_at) AS created_at
        FROM usage_logs
        WHERE actual_cost > 0
        GROUP BY account_id
    ) latest ON latest.account_id = ul.account_id AND latest.created_at = ul.created_at
    WHERE ul.actual_cost > 0
    ORDER BY ul.account_id, ul.id DESC
), recent_group_usage AS MATERIALIZED (
    SELECT ul.group_id, ul.account_id, COUNT(*)::bigint AS request_count
    FROM usage_logs ul
    WHERE ul.group_id IS NOT NULL
      AND ul.actual_cost > 0
      AND ul.created_at >= NOW() - INTERVAL '24 hours'
    GROUP BY ul.group_id, ul.account_id
), latest_account_probe AS MATERIALIZED (
    SELECT DISTINCT ON (mc.target_key) mc.target_key, mc.checked_at, mc.status,
           mc.error_class, mc.status_code
    FROM monitoring_checks mc
    WHERE mc.source = 'probe' AND mc.target_key LIKE 'account:%'
    ORDER BY mc.target_key, mc.checked_at DESC, mc.id DESC
), active_channel_groups AS MATERIALIZED (
    SELECT DISTINCT cg.group_id
    FROM channel_groups cg
    JOIN channels c ON c.id = cg.channel_id
    WHERE LOWER(TRIM(c.status)) = 'active'
)
SELECT a.id, a.name, a.platform, a.type, a.status, a.schedulable, a.priority, a.credentials,
       a.updated_at,
       persisted_target.source_fingerprint, persisted_target.source_updated_at,
       a.proxy_id, p.protocol, p.host, p.port, p.username, p.password, p.status,
       recent.created_at, recent.model,
       last_probe.checked_at, last_probe.status, last_probe.error_class, last_probe.status_code,
       channel_error.created_at, channel_error.error_class, channel_error.status_code,
       COALESCE(alert_state.failure_streak, 0), alert_state.updated_at,
       ag.priority, COALESCE(member_usage.request_count, 0),
	       g.id, g.name, g.platform, g.status, g.updated_at,
       active_channel.group_id IS NOT NULL
FROM accounts a
LEFT JOIN proxies p ON p.id = a.proxy_id
    AND p.deleted_at IS NULL
    AND LOWER(TRIM(p.status)) = 'active'
    AND (p.expires_at IS NULL OR p.expires_at > NOW())
LEFT JOIN recent_account_usage recent ON recent.account_id = a.id
LEFT JOIN latest_account_probe last_probe
       ON last_probe.target_key = 'account:' || a.id::text
LEFT JOIN monitoring_targets persisted_target
       ON persisted_target.target_key = 'account:' || a.id::text
      AND persisted_target.kind = 'account'
      AND persisted_target.active = TRUE
LEFT JOIN LATERAL (
    SELECT channel_errors.created_at, channel_errors.error_class, channel_errors.status_code
    FROM (
        SELECT oe.created_at,
               COALESCE(
                   NULLIF(BTRIM(oe.error_type), ''),
                   NULLIF(BTRIM(oe.error_phase), ''),
                   'upstream_error'
               ) AS error_class,
               COALESCE(NULLIF(oe.upstream_status_code, 0), oe.status_code) AS status_code,
               oe.id AS error_id
        FROM ops_error_logs oe
        WHERE oe.account_id = a.id
          AND oe.created_at >= NOW() - INTERVAL '24 hours'
          AND (persisted_target.source_updated_at IS NULL
               OR oe.created_at >= persisted_target.source_updated_at - INTERVAL '2 minutes')
          AND (persisted_target.last_channel_error_resolved_at IS NULL
               OR oe.created_at > persisted_target.last_channel_error_resolved_at)
          AND COALESCE(oe.is_business_limited, FALSE) = FALSE
          AND LOWER(BTRIM(COALESCE(oe.error_type, ''))) NOT IN (
              'cyber_policy', 'client_cancelled', 'rate_limit_error', 'invalid_request_error'
          )
          AND LOWER(BTRIM(COALESCE(oe.error_owner, ''))) = 'provider'
          AND (
              LOWER(BTRIM(COALESCE(oe.error_phase, ''))) IN ('account_auth', 'network', 'upstream')
              OR LOWER(BTRIM(COALESCE(oe.error_source, ''))) IN ('upstream_http', 'upstream_network')
          )
          AND COALESCE(NULLIF(oe.upstream_status_code, 0), oe.status_code, 0) <> 429
        UNION ALL
        SELECT persisted_target.last_channel_error_at,
               NULLIF(BTRIM(persisted_target.last_channel_error_class), ''),
               persisted_target.last_channel_error_status_code,
               0
        WHERE persisted_target.last_channel_error_at IS NOT NULL
          AND (persisted_target.source_updated_at IS NULL
               OR persisted_target.last_channel_error_at >= persisted_target.source_updated_at - INTERVAL '2 minutes')
          AND (persisted_target.last_channel_error_resolved_at IS NULL
               OR persisted_target.last_channel_error_at > persisted_target.last_channel_error_resolved_at)
    ) channel_errors
    ORDER BY channel_errors.created_at DESC, channel_errors.error_id DESC
    LIMIT 1
) channel_error ON TRUE
LEFT JOIN monitoring_alert_states alert_state
       ON alert_state.target_key = 'account:' || a.id::text
LEFT JOIN account_groups ag ON ag.account_id = a.id
LEFT JOIN recent_group_usage member_usage
       ON member_usage.group_id = ag.group_id
      AND member_usage.account_id = a.id
LEFT JOIN groups g ON g.id = ag.group_id AND g.deleted_at IS NULL
LEFT JOIN active_channel_groups active_channel ON active_channel.group_id = g.id
WHERE a.deleted_at IS NULL
ORDER BY a.id, g.id`

const groupSnapshotQuery = `
SELECT g.id, g.name, g.platform, g.status, g.updated_at,
       EXISTS (
           SELECT 1
           FROM channel_groups cg
           JOIN channels c ON c.id = cg.channel_id
           WHERE cg.group_id = g.id
             AND LOWER(TRIM(c.status)) = 'active'
       )
FROM groups g
WHERE g.deleted_at IS NULL`

type snapshotRow struct {
	id, groupID, proxyID                  sql.NullInt64
	name, platform, accountType, status   sql.NullString
	accountPriority                       sql.NullInt64
	proxyProtocol, proxyHost, proxyUser   sql.NullString
	proxyPassword, proxyStatus            sql.NullString
	proxyPort                             sql.NullInt64
	updatedAt, lastActivity, lastProbe    sql.NullTime
	persistedSourceFingerprint            sql.NullString
	persistedSourceUpdatedAt              sql.NullTime
	lastProbeStatus, lastProbeErrorClass  sql.NullString
	lastProbeStatusCode                   sql.NullInt64
	lastChannelError                      sql.NullTime
	lastChannelErrorClass                 sql.NullString
	lastChannelErrorCode                  sql.NullInt64
	probeFailureStreak                    sql.NullInt64
	alertStateUpdatedAt                   sql.NullTime
	recentModel                           sql.NullString
	groupPriority, groupRequestCount      sql.NullInt64
	schedulable                           bool
	groupHasActiveChannel                 bool
	credentials                           []byte
	groupName, groupPlatform, groupStatus sql.NullString
	groupUpdatedAt                        sql.NullTime
}

// LoadSnapshot 只读取网关账户、分组和最近成功请求，不修改业务表或监控表。
func (s *Store) LoadSnapshot(ctx context.Context) (model.Snapshot, error) {
	accounts := make(map[int64]*model.Account)
	groups := make(map[int64]*model.Group)
	if err := s.loadAccountSnapshot(ctx, accounts, groups); err != nil {
		return model.Snapshot{}, err
	}
	if err := s.loadGroups(ctx, groups); err != nil {
		return model.Snapshot{}, err
	}
	return buildSnapshot(accounts, groups), nil
}

func (s *Store) loadAccountSnapshot(ctx context.Context, accounts map[int64]*model.Account, groups map[int64]*model.Group) error {
	rows, err := s.db.QueryContext(ctx, snapshotQuery)
	if err != nil {
		return fmt.Errorf("load account snapshot: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var row snapshotRow
		if err := row.scan(rows); err != nil {
			return fmt.Errorf("scan account snapshot: %w", err)
		}
		if err := mergeSnapshotRow(row, accounts, groups); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

type groupSnapshotRow struct {
	groupID          sql.NullInt64
	name, platform   sql.NullString
	status           sql.NullString
	updatedAt        sql.NullTime
	hasActiveChannel bool
}

func (s *Store) loadGroups(ctx context.Context, groups map[int64]*model.Group) error {
	rows, err := s.db.QueryContext(ctx, groupSnapshotQuery)
	if err != nil {
		return fmt.Errorf("load group snapshot: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var row groupSnapshotRow
		if err := rows.Scan(&row.groupID, &row.name, &row.platform, &row.status, &row.updatedAt, &row.hasActiveChannel); err != nil {
			return fmt.Errorf("scan group snapshot: %w", err)
		}
		mergeGroupSnapshotRow(row, groups)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read group snapshot: %w", err)
	}
	return nil
}

func mergeGroupSnapshotRow(row groupSnapshotRow, groups map[int64]*model.Group) {
	if !row.groupID.Valid {
		return
	}
	group := groups[row.groupID.Int64]
	if group == nil {
		group = &model.Group{ID: row.groupID.Int64}
		groups[group.ID] = group
	}
	group.Name = row.name.String
	group.Platform = row.platform.String
	group.Status = row.status.String
	group.HasActiveChannel = row.hasActiveChannel
	if row.updatedAt.Valid {
		value := row.updatedAt.Time.UTC()
		group.UpdatedAt = &value
	} else {
		group.UpdatedAt = nil
	}
}

func (r *snapshotRow) scan(rows *sql.Rows) error {
	return rows.Scan(
		&r.id, &r.name, &r.platform, &r.accountType, &r.status, &r.schedulable, &r.accountPriority, &r.credentials,
		&r.updatedAt,
		&r.persistedSourceFingerprint, &r.persistedSourceUpdatedAt,
		&r.proxyID, &r.proxyProtocol, &r.proxyHost, &r.proxyPort, &r.proxyUser, &r.proxyPassword,
		&r.proxyStatus, &r.lastActivity, &r.recentModel, &r.lastProbe,
		&r.lastProbeStatus, &r.lastProbeErrorClass, &r.lastProbeStatusCode,
		&r.lastChannelError, &r.lastChannelErrorClass, &r.lastChannelErrorCode,
		&r.probeFailureStreak,
		&r.alertStateUpdatedAt,
		&r.groupPriority, &r.groupRequestCount, &r.groupID, &r.groupName,
		&r.groupPlatform, &r.groupStatus, &r.groupUpdatedAt, &r.groupHasActiveChannel,
	)
}

func mergeSnapshotRow(row snapshotRow, accounts map[int64]*model.Account, groups map[int64]*model.Group) error {
	if !row.id.Valid {
		return nil
	}
	account := accounts[row.id.Int64]
	if account == nil {
		var err error
		account, err = row.account()
		if err != nil {
			return err
		}
		accounts[account.ID] = account
	}
	if row.groupID.Valid {
		linkGroup(row, account, groups)
	}
	return nil
}

func (r snapshotRow) account() (*model.Account, error) {
	account := &model.Account{
		ID: r.id.Int64, Name: r.name.String, Platform: r.platform.String,
		Type: r.accountType.String, Status: r.status.String,
		Schedulable: r.schedulable, Credentials: map[string]any{},
	}
	effectiveUpdatedAt := effectiveAccountUpdatedAt(r.updatedAt, r.lastActivity)
	if r.accountPriority.Valid {
		account.Priority = int(r.accountPriority.Int64)
	}
	if len(r.credentials) > 0 {
		if err := json.Unmarshal(r.credentials, &account.Credentials); err != nil {
			return nil, fmt.Errorf("decode credentials for account %d: %w", account.ID, err)
		}
	}
	if r.lastActivity.Valid {
		value := r.lastActivity.Time.UTC()
		account.LastActivityAt = &value
	}
	if effectiveUpdatedAt.Valid {
		value := effectiveUpdatedAt.Time.UTC()
		account.UpdatedAt = &value
	}
	if r.lastProbe.Valid {
		value := r.lastProbe.Time.UTC()
		account.LastProbeAt = &value
	}
	account.LastProbeStatus = strings.TrimSpace(r.lastProbeStatus.String)
	account.LastProbeErrorClass = strings.TrimSpace(r.lastProbeErrorClass.String)
	if r.lastProbeStatusCode.Valid {
		value := int(r.lastProbeStatusCode.Int64)
		account.LastProbeStatusCode = &value
	}
	if r.lastChannelError.Valid {
		value := r.lastChannelError.Time.UTC()
		if !channelErrorResolved(value, account.LastActivityAt, account.LastProbeAt, account.LastProbeStatus) {
			account.LastChannelErrorAt = &value
		}
	}
	if account.LastChannelErrorAt != nil {
		account.LastChannelErrorClass = strings.TrimSpace(r.lastChannelErrorClass.String)
	}
	if account.LastChannelErrorAt != nil && r.lastChannelErrorCode.Valid {
		value := int(r.lastChannelErrorCode.Int64)
		account.LastChannelErrorStatusCode = &value
	}
	if r.recentModel.Valid {
		account.RecentModel = strings.TrimSpace(r.recentModel.String)
	}
	account.ChatGPTAccount = credentialString(account.Credentials, "chatgpt_account_id")
	if r.proxyID.Valid {
		applyProxy(account, r)
	}
	account.SourceFingerprint = accountSourceFingerprint(*account)
	account.SourceUpdatedAt = resolveSnapshotSourceUpdatedAtForFingerprint(
		r.persistedSourceFingerprint, r.persistedSourceUpdatedAt,
		account.SourceFingerprint, accountSourceCandidate(*account),
	)
	if alertStateCurrentForSource(
		r.alertStateUpdatedAt, nullableTime(account.SourceUpdatedAt), r.lastActivity,
	) {
		account.ProbeFailureStreak = nullInt(r.probeFailureStreak)
	}
	return account, nil
}

func resolveSnapshotSourceUpdatedAtForFingerprint(
	persistedFingerprint sql.NullString,
	persistedSourceUpdatedAt sql.NullTime,
	currentFingerprint string,
	candidate *time.Time,
) *time.Time {
	if !persistedFingerprint.Valid {
		return cloneTime(candidate)
	}
	fingerprint := strings.TrimSpace(persistedFingerprint.String)
	if !currentSourceFingerprint(fingerprint) {
		// Rebaseline fingerprints written by an older implementation. The
		// target sync will persist the new identity without filtering history.
		return nil
	}
	if fingerprint == currentFingerprint {
		return cloneNullableTime(persistedSourceUpdatedAt)
	}
	return cloneTime(candidate)
}

func cloneNullableTime(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time.UTC()
	return &result
}

func nullableTime(value *time.Time) sql.NullTime {
	if value == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: value.UTC(), Valid: true}
}

func channelErrorResolved(errorAt time.Time, activityAt, probeAt *time.Time, probeStatus string) bool {
	if activityAt != nil && !activityAt.Before(errorAt) {
		return true
	}
	return probeAt != nil && !probeAt.Before(errorAt) &&
		(probeStatus == model.StatusOperational || probeStatus == model.StatusDegraded)
}

func effectiveAccountUpdatedAt(updatedAt, lastActivity sql.NullTime) sql.NullTime {
	if !updatedAt.Valid {
		return updatedAt
	}
	if !lastActivity.Valid {
		return updatedAt
	}
	updated := updatedAt.Time.UTC()
	activity := lastActivity.Time.UTC()
	effective := effectiveSourceUpdatedAt(&updated, &activity)
	if effective == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *effective, Valid: true}
}

func alertStateCurrent(stateUpdatedAt, accountUpdatedAt sql.NullTime) bool {
	if !stateUpdatedAt.Valid {
		return false
	}
	if !accountUpdatedAt.Valid {
		return true
	}
	return !stateUpdatedAt.Time.Before(accountUpdatedAt.Time)
}

func alertStateCurrentForSource(stateUpdatedAt, sourceUpdatedAt, activityAt sql.NullTime) bool {
	if !stateUpdatedAt.Valid {
		return false
	}
	if !sourceUpdatedAt.Valid {
		return true
	}
	// source_updated_at is set to last_activity_at when the gateway's row
	// update is only the bookkeeping side effect of a successful request. That
	// activity must be allowed to consume an earlier probe failure and emit a
	// recovery alert.
	if activityAt.Valid && sourceUpdatedAt.Time.Equal(activityAt.Time) {
		return true
	}
	return !stateUpdatedAt.Time.Before(sourceUpdatedAt.Time)
}

func applyProxy(account *model.Account, row snapshotRow) {
	if !row.proxyProtocol.Valid || !row.proxyHost.Valid || !row.proxyPort.Valid ||
		!strings.EqualFold(strings.TrimSpace(row.proxyStatus.String), "active") {
		account.ProxyError = "configured proxy is unavailable"
		return
	}
	account.ProxyURL = buildProxyURL(
		row.proxyProtocol.String,
		row.proxyHost.String,
		int(row.proxyPort.Int64),
		row.proxyUser.String,
		row.proxyPassword.String,
	)
}

func linkGroup(row snapshotRow, account *model.Account, groups map[int64]*model.Group) {
	if !containsID(account.GroupIDs, row.groupID.Int64) {
		account.GroupIDs = append(account.GroupIDs, row.groupID.Int64)
	}
	group := groups[row.groupID.Int64]
	if group == nil {
		group = &model.Group{
			ID: row.groupID.Int64, Name: row.groupName.String,
			Platform: row.groupPlatform.String, Status: row.groupStatus.String,
			HasActiveChannel: row.groupHasActiveChannel,
		}
		if row.groupUpdatedAt.Valid {
			value := row.groupUpdatedAt.Time.UTC()
			group.UpdatedAt = &value
		}
		groups[group.ID] = group
	}
	group.HasActiveChannel = group.HasActiveChannel || row.groupHasActiveChannel
	if !containsID(group.AccountIDs, account.ID) {
		group.AccountIDs = append(group.AccountIDs, account.ID)
	}
	member := model.GroupMember{
		AccountID:       account.ID,
		GroupPriority:   nullInt(row.groupPriority),
		AccountPriority: account.Priority,
		RequestCount:    nullInt64(row.groupRequestCount),
	}
	for index := range group.Members {
		if group.Members[index].AccountID == account.ID {
			group.Members[index] = member
			return
		}
	}
	group.Members = append(group.Members, member)
}

func buildSnapshot(accounts map[int64]*model.Account, groups map[int64]*model.Group) model.Snapshot {
	activeGroupIDs := make(map[int64]struct{}, len(groups))
	for id, group := range groups {
		if groupIsActive(group.Status) {
			activeGroupIDs[id] = struct{}{}
		}
	}
	activeAccounts := make(map[int64]*model.Account, len(accounts))
	for id, account := range accounts {
		if !accountIsMonitored(*account) {
			continue
		}
		copy := *account
		copy.GroupIDs = filterIDs(account.GroupIDs, activeGroupIDs)
		activeAccounts[id] = &copy
	}
	enabledAccounts := make(map[int64]*model.Account, len(activeAccounts))
	for id, account := range activeAccounts {
		if accountIsEnabled(*account) {
			enabledAccounts[id] = account
		}
	}
	activeGroups := make(map[int64]*model.Group, len(groups))
	for id, group := range groups {
		if !groupIsActive(group.Status) {
			continue
		}
		copy := *group
		// Keep source changes from members that are filtered out below. A
		// disabled/error member must still invalidate older group evidence.
		copy.UpdatedAt = groupSourceCandidate(*group, accounts)
		copy.AccountIDs = filterAccountIDs(group.AccountIDs, enabledAccounts)
		copy.Members = filterGroupMembers(*group, enabledAccounts)
		copy.ProbeEnabled = groupProbeEnabled(copy, enabledAccounts)
		fingerprintGroup := *group
		fingerprintGroup.ProbeEnabled = copy.ProbeEnabled
		copy.SourceFingerprint = groupSourceFingerprint(fingerprintGroup, accounts)
		activeGroups[id] = &copy
	}

	accountList := make([]model.Account, 0, len(activeAccounts))
	for _, account := range activeAccounts {
		accountList = append(accountList, *account)
	}
	sort.Slice(accountList, func(i, j int) bool {
		leftName, rightName := strings.TrimSpace(accountList[i].Name), strings.TrimSpace(accountList[j].Name)
		if leftName != rightName {
			return leftName < rightName
		}
		return accountList[i].ID < accountList[j].ID
	})
	groupList := make([]model.Group, 0, len(activeGroups))
	for _, group := range activeGroups {
		groupList = append(groupList, *group)
	}
	sort.Slice(groupList, func(i, j int) bool {
		leftName, rightName := strings.TrimSpace(groupList[i].Name), strings.TrimSpace(groupList[j].Name)
		if leftName != rightName {
			return leftName < rightName
		}
		return groupList[i].ID < groupList[j].ID
	})

	snapshot := model.Snapshot{Accounts: accountList, Groups: groupList}
	return snapshot
}

func groupProbeEnabled(group model.Group, accounts map[int64]*model.Account) bool {
	if !groupIsActive(group.Status) || !group.HasActiveChannel {
		return false
	}
	for _, accountID := range group.AccountIDs {
		if account, ok := accounts[accountID]; ok && accountIsEnabled(*account) {
			return true
		}
	}
	return false
}

func accountIsActive(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "active")
}

func accountIsEnabled(account model.Account) bool {
	return accountIsActive(account.Status) && account.Schedulable
}

// accountIsMonitored 保留可调度账户和可调度的错误账户；禁用账户不进入
// 监控、诊断或恢复探测。
func accountIsMonitored(account model.Account) bool {
	return account.Schedulable &&
		(strings.EqualFold(strings.TrimSpace(account.Status), "error") || accountIsActive(account.Status))
}

func filterIDs(values []int64, allowed map[int64]struct{}) []int64 {
	filtered := make([]int64, 0, len(values))
	for _, value := range values {
		if _, ok := allowed[value]; ok {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func filterAccountIDs(values []int64, accounts map[int64]*model.Account) []int64 {
	filtered := make([]int64, 0, len(values))
	for _, value := range values {
		if _, ok := accounts[value]; ok {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func filterGroupMembers(group model.Group, accounts map[int64]*model.Account) []model.GroupMember {
	members := group.Members
	if len(members) > 0 {
		members = append([]model.GroupMember(nil), members...)
	}
	seen := make(map[int64]struct{}, len(members))
	for _, member := range members {
		seen[member.AccountID] = struct{}{}
	}
	for _, accountID := range group.AccountIDs {
		if _, exists := seen[accountID]; exists {
			continue
		}
		account, ok := accounts[accountID]
		if !ok {
			continue
		}
		members = append(members, model.GroupMember{AccountID: accountID, AccountPriority: account.Priority})
		seen[accountID] = struct{}{}
	}
	filtered := make([]model.GroupMember, 0, len(members))
	for _, member := range members {
		if _, ok := accounts[member.AccountID]; !ok {
			continue
		}
		if account, ok := accounts[member.AccountID]; ok {
			member.AccountPriority = account.Priority
		}
		filtered = append(filtered, member)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].AccountPriority != filtered[j].AccountPriority {
			return filtered[i].AccountPriority < filtered[j].AccountPriority
		}
		if filtered[i].GroupPriority != filtered[j].GroupPriority {
			return filtered[i].GroupPriority < filtered[j].GroupPriority
		}
		return filtered[i].AccountID < filtered[j].AccountID
	})
	return filtered
}

func nullInt(value sql.NullInt64) int {
	if !value.Valid {
		return 0
	}
	return int(value.Int64)
}

func nullInt64(value sql.NullInt64) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

func containsID(values []int64, value int64) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func credentialString(credentials map[string]any, key string) string {
	value, _ := credentials[key].(string)
	return strings.TrimSpace(value)
}

func accountHistoryEligible(account model.Account) bool {
	status := strings.TrimSpace(account.Status)
	return strings.EqualFold(status, "active") || strings.EqualFold(status, "error")
}

func groupIsActive(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "active")
}

func buildProxyURL(protocol, host string, port int, username, password string) string {
	proxyURL := &url.URL{
		Scheme: strings.TrimSpace(protocol),
		Host:   net.JoinHostPort(strings.TrimSpace(host), strconv.Itoa(port)),
	}
	if username != "" {
		proxyURL.User = url.UserPassword(username, password)
	}
	return proxyURL.String()
}
