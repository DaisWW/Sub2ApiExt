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

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

const snapshotQuery = `
SELECT a.id, a.name, a.platform, a.type, a.status, a.schedulable, a.credentials,
       a.proxy_id, p.protocol, p.host, p.port, p.username, p.password, p.status,
       recent.created_at, recent.model,
       g.id, g.name, g.platform, g.status
FROM accounts a
LEFT JOIN proxies p ON p.id = a.proxy_id AND p.deleted_at IS NULL
LEFT JOIN LATERAL (
    SELECT ul.created_at, ul.model
    FROM usage_logs ul
	WHERE ul.account_id = a.id AND ul.actual_cost > 0
    ORDER BY ul.created_at DESC, ul.id DESC
    LIMIT 1
) recent ON TRUE
LEFT JOIN account_groups ag ON ag.account_id = a.id
LEFT JOIN groups g ON g.id = ag.group_id AND g.deleted_at IS NULL
WHERE a.deleted_at IS NULL
ORDER BY a.id, g.id`

type snapshotRow struct {
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
}

// LoadSnapshot 只读取网关账户、分组和最近成功请求，不修改业务表或监控表。
func (s *Store) LoadSnapshot(ctx context.Context) (model.Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, snapshotQuery)
	if err != nil {
		return model.Snapshot{}, fmt.Errorf("load account snapshot: %w", err)
	}
	defer rows.Close()

	accounts := make(map[int64]*model.Account)
	groups := make(map[int64]*model.Group)
	for rows.Next() {
		var row snapshotRow
		if err := row.scan(rows); err != nil {
			return model.Snapshot{}, fmt.Errorf("scan account snapshot: %w", err)
		}
		if err := mergeSnapshotRow(row, accounts, groups); err != nil {
			return model.Snapshot{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return model.Snapshot{}, err
	}
	return buildSnapshot(accounts, groups), nil
}

func (r *snapshotRow) scan(rows *sql.Rows) error {
	return rows.Scan(
		&r.id, &r.name, &r.platform, &r.accountType, &r.status, &r.schedulable, &r.credentials,
		&r.proxyID, &r.proxyProtocol, &r.proxyHost, &r.proxyPort, &r.proxyUser, &r.proxyPassword,
		&r.proxyStatus, &r.lastActivity, &r.recentModel, &r.groupID, &r.groupName,
		&r.groupPlatform, &r.groupStatus,
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
	if len(r.credentials) > 0 {
		if err := json.Unmarshal(r.credentials, &account.Credentials); err != nil {
			return nil, fmt.Errorf("decode credentials for account %d: %w", account.ID, err)
		}
	}
	if r.lastActivity.Valid {
		value := r.lastActivity.Time.UTC()
		account.LastActivityAt = &value
	}
	if r.recentModel.Valid {
		account.RecentModel = strings.TrimSpace(r.recentModel.String)
	}
	account.ChatGPTAccount = credentialString(account.Credentials, "chatgpt_account_id")
	if r.proxyID.Valid {
		applyProxy(account, r)
	}
	return account, nil
}

func applyProxy(account *model.Account, row snapshotRow) {
	if !row.proxyProtocol.Valid || !row.proxyHost.Valid || !row.proxyPort.Valid || row.proxyStatus.String != "active" {
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
		}
		groups[group.ID] = group
	}
	if !containsID(group.AccountIDs, account.ID) {
		group.AccountIDs = append(group.AccountIDs, account.ID)
	}
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
	activeGroups := make(map[int64]*model.Group, len(groups))
	for id, group := range groups {
		if !groupIsActive(group.Status) {
			continue
		}
		copy := *group
		copy.AccountIDs = filterAccountIDs(group.AccountIDs, activeAccounts)
		copy.ProbeEnabled = groupProbeEnabled(copy, activeAccounts)
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
	if !groupIsActive(group.Status) {
		return false
	}
	for _, accountID := range group.AccountIDs {
		if account, ok := accounts[accountID]; ok && accountIsMonitored(*account) {
			return true
		}
	}
	return false
}

func accountIsActive(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "active")
}

// accountIsMonitored 保留启用账户和错误账户；错误账户仅用于恢复探测，不进入启用统计。
func accountIsMonitored(account model.Account) bool {
	return account.Status == "error" || (accountIsActive(account.Status) && account.Schedulable)
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
	return account.Status == "active" || account.Status == "error"
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
