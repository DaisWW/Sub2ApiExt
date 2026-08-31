package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *PostgresChannelSource) ListGroupUsageStats(ctx context.Context, start, end time.Time) ([]GroupUsageStats, error) {
	rows, err := s.db.QueryContext(ctx, groupUsageStatsSQL, start, end)
	if err != nil {
		return nil, fmt.Errorf("查询分组历史用量: %w", err)
	}
	defer rows.Close()

	var results []GroupUsageStats
	for rows.Next() {
		var row GroupUsageStats
		if err := rows.Scan(&row.GroupID, &row.Requests, &row.StandardCost, &row.AccountCost); err != nil {
			return nil, fmt.Errorf("读取分组历史用量: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历分组历史用量: %w", err)
	}
	return results, nil
}

func (s *PostgresChannelSource) ListGroupUsageStatsByWindows(ctx context.Context, end time.Time, windows []time.Duration) ([]GroupUsageWindowStats, error) {
	values, err := historyWindowValues(windows)
	if err != nil || len(values) == 0 {
		return nil, err
	}
	query := fmt.Sprintf(groupUsageStatsByWindowsSQL, strings.Join(values, ","))
	rows, err := s.db.QueryContext(ctx, query, end)
	if err != nil {
		return nil, fmt.Errorf("查询分组多窗口历史用量: %w", err)
	}
	defer rows.Close()

	var results []GroupUsageWindowStats
	for rows.Next() {
		var seconds int64
		var result GroupUsageWindowStats
		if err := rows.Scan(&seconds, &result.GroupID, &result.Requests, &result.StandardCost, &result.AccountCost); err != nil {
			return nil, fmt.Errorf("读取分组多窗口历史用量: %w", err)
		}
		result.Window = time.Duration(seconds) * time.Second
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历分组多窗口历史用量: %w", err)
	}
	return results, nil
}

func historyWindowValues(windows []time.Duration) ([]string, error) {
	values := make([]string, 0, len(windows))
	seen := make(map[int64]struct{}, len(windows))
	for _, window := range windows {
		seconds := int64(window / time.Second)
		if seconds <= 0 {
			return nil, fmt.Errorf("历史统计窗口无效: %s", window)
		}
		if _, exists := seen[seconds]; exists {
			continue
		}
		seen[seconds] = struct{}{}
		// 秒数来自已经校验过的 time.Duration，不会把用户文本直接拼入 SQL。
		values = append(values, "("+strconv.FormatInt(seconds, 10)+")")
	}
	return values, nil
}

func (s *PostgresChannelSource) LatestGroupUsageID(ctx context.Context) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, latestGroupUsageIDSQL).Scan(&id); err != nil {
		return 0, fmt.Errorf("查询用量日志水位: %w", err)
	}
	return id, nil
}

func (s *PostgresChannelSource) ListGroupUsageSince(ctx context.Context, watermarks map[int64]int64, throughID int64) ([]GroupUsageAccountStats, error) {
	if len(watermarks) == 0 {
		return nil, nil
	}
	groupIDs := sortedGroupIDs(watermarks)
	values := make([]string, 0, len(groupIDs))
	args := make([]any, 0, len(groupIDs)*2+1)
	for _, groupID := range groupIDs {
		args = append(args, groupID, watermarks[groupID])
		values = append(values, fmt.Sprintf("($%d::bigint, $%d::bigint)", len(args)-1, len(args)))
	}
	args = append(args, throughID)
	query := fmt.Sprintf(groupUsageSinceSQL, strings.Join(values, ","), fmt.Sprintf("$%d", len(args)))
	return s.queryGroupUsageAccounts(ctx, query, args, "查询分组增量用量")
}

func (s *PostgresChannelSource) ListGroupUsageAccounts(ctx context.Context, start, end time.Time, throughID int64, groupIDs []int64) ([]GroupUsageAccountStats, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	groupIDs = append([]int64(nil), groupIDs...)
	sort.Slice(groupIDs, func(i, j int) bool { return groupIDs[i] < groupIDs[j] })
	placeholders := make([]string, 0, len(groupIDs))
	args := make([]any, 0, len(groupIDs)+3)
	args = append(args, start, end, throughID)
	for _, groupID := range groupIDs {
		args = append(args, groupID)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	query := fmt.Sprintf(groupUsageAccountsSQL, strings.Join(placeholders, ","))
	return s.queryGroupUsageAccounts(ctx, query, args, "查询分组初始化用量")
}

func (s *PostgresChannelSource) queryGroupUsageAccounts(ctx context.Context, query string, args []any, action string) ([]GroupUsageAccountStats, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer rows.Close()

	var results []GroupUsageAccountStats
	for rows.Next() {
		var row GroupUsageAccountStats
		if err := rows.Scan(&row.GroupID, &row.AccountID, &row.Requests, &row.StandardCost, &row.BaseCost, &row.AccountCost, &row.CurrentAccountRate); err != nil {
			return nil, fmt.Errorf("%s结果: %w", action, err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历%s结果: %w", action, err)
	}
	return results, nil
}

func sortedGroupIDs(values map[int64]int64) []int64 {
	result := make([]int64, 0, len(values))
	for groupID := range values {
		result = append(result, groupID)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
