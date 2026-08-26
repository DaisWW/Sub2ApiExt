package main

import (
	"context"
	"time"
)

func loadDynamicBootstrap(
	ctx context.Context,
	source groupUsageIncrementalSource,
	now time.Time,
	snapshotID int64,
	groupIDs []int64,
) (map[int64]dynamicBootstrapChoice, []int64, error) {
	choices := make(map[int64]dynamicBootstrapChoice)
	if len(groupIDs) == 0 {
		return choices, nil, nil
	}
	unresolved := append([]int64(nil), groupIDs...)
	for _, window := range dynamicBootstrapWindowOrder {
		if len(unresolved) == 0 {
			break
		}
		rows, err := source.ListGroupUsageAccounts(
			ctx,
			now.Add(-window),
			now,
			snapshotID,
			unresolved,
		)
		if err != nil {
			return nil, nil, err
		}
		usageByGroup := groupDynamicUsage(rows)
		next := make([]int64, 0, len(unresolved))
		for _, groupID := range unresolved {
			groupRows := usageByGroup[groupID]
			if dynamicBootstrapSufficient(groupRows) {
				choices[groupID] = dynamicBootstrapChoice{rows: groupRows, window: window}
				continue
			}
			next = append(next, groupID)
		}
		unresolved = next
	}
	return choices, unresolved, nil
}
