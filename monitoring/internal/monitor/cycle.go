package monitor

import (
	"context"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/probe"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/stats"
)

type cycleBatch struct {
	accountResults   map[int64]model.ProbeResult
	observations     []model.ProbeResult
	persisted        []model.ProbeResult
	passiveAccounts  int
	deferredAccounts int
	verifiedAccounts int
}

const recoveryProbeInterval = 15 * time.Minute

type accountEvidence struct {
	status    string
	source    string
	checkedAt *time.Time
	valid     bool
}

func (s *Service) runCycle(ctx context.Context) error {
	release, acquired, err := s.store.AcquireCycleLease(ctx)
	if err != nil {
		return err
	}
	if !acquired {
		s.log.Debug("monitoring cycle skipped; another instance owns the lease")
		return nil
	}
	defer release()

	snapshot, err := s.store.LoadSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := s.store.SyncTargets(ctx, snapshot); err != nil {
		return err
	}
	cycleNow := time.Now().UTC()
	batch, probeAccounts := newCycleBatch(snapshot, cycleNow, s.cfg.Interval)
	probeResults, err := runProbes(ctx, probeAccounts, s.cfg.ProbeConcurrency, s.prober.Probe)
	if err != nil {
		return err
	}
	batch.addProbeResults(probeResults)
	batch.aggregateGroups(snapshot, indexAccounts(snapshot.Accounts), cycleNow)
	if err := s.store.InsertResults(ctx, batch.persisted); err != nil {
		return err
	}
	s.evaluateAlerts(ctx, batch.observations, targetNames(snapshot))
	s.maybePrune(ctx)
	s.logCycle(snapshot, probeAccounts, batch)
	return nil
}

func newCycleBatch(snapshot model.Snapshot, now time.Time, interval time.Duration) (*cycleBatch, []model.Account) {
	batch := &cycleBatch{
		accountResults: make(map[int64]model.ProbeResult),
		observations:   make([]model.ProbeResult, 0, len(snapshot.Accounts)+len(snapshot.Groups)),
		persisted:      make([]model.ProbeResult, 0, len(snapshot.Accounts)+len(snapshot.Groups)),
	}
	probeAccounts := make([]model.Account, 0, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		if !probeEligible(account) {
			continue
		}
		evidence := latestAccountEvidence(account)
		if evidence.valid && successfulEvidence(evidence.status) {
			if shouldObserveHistoryRecovery(account, evidence, now, interval) {
				batch.addPassiveAccount(account)
			} else {
				batch.addCachedEvidence(account.ID, evidence)
			}
			batch.verifiedAccounts++
			continue
		}
		if !probe.SupportsAccount(account) {
			continue
		}
		probeInterval := max(interval, recoveryProbeInterval)
		if evidence.valid && evidence.checkedAt != nil && !probeDue(evidence.checkedAt, now, probeInterval) {
			batch.addCachedEvidence(account.ID, evidence)
			batch.deferredAccounts++
			continue
		}
		probeAccounts = append(probeAccounts, account)
	}
	return batch, probeAccounts
}

func latestAccountEvidence(account model.Account) accountEvidence {
	var evidence accountEvidence
	if evidenceTimeValid(account.LastActivityAt, account.UpdatedAt) {
		evidence = accountEvidence{
			status: model.StatusOperational, source: "history", checkedAt: account.LastActivityAt, valid: true,
		}
	}
	if evidenceTimeValid(account.LastProbeAt, account.UpdatedAt) &&
		(evidence.checkedAt == nil || account.LastProbeAt.After(*evidence.checkedAt)) {
		evidence = accountEvidence{
			status: account.LastProbeStatus, source: "probe", checkedAt: account.LastProbeAt,
			valid: probeEvidenceStatus(account.LastProbeStatus),
		}
	}
	return evidence
}

func evidenceTimeValid(value, changedAt *time.Time) bool {
	return value != nil && (changedAt == nil || !value.Before(*changedAt))
}

func successfulEvidence(status string) bool {
	return status == model.StatusOperational || status == model.StatusDegraded
}

func probeEvidenceStatus(status string) bool {
	return successfulEvidence(status) || status == model.StatusFailed || status == model.StatusError
}

func shouldObserveHistoryRecovery(account model.Account, evidence accountEvidence, now time.Time, interval time.Duration) bool {
	if evidence.source != "history" || evidence.checkedAt == nil {
		return false
	}
	if now.Sub(*evidence.checkedAt) < interval {
		return true
	}
	return account.LastProbeAt != nil && evidence.checkedAt.After(*account.LastProbeAt) &&
		(account.LastProbeStatus == model.StatusFailed || account.LastProbeStatus == model.StatusError)
}

func probeDue(lastProbeAt *time.Time, now time.Time, interval time.Duration) bool {
	return lastProbeAt == nil || !lastProbeAt.Add(interval).After(now)
}

func (b *cycleBatch) addPassiveAccount(account model.Account) {
	result := model.ProbeResult{
		TargetKey: model.TargetKey(model.KindAccount, account.ID),
		Kind:      model.KindAccount,
		EntityID:  account.ID,
		Status:    model.StatusOperational,
		CheckedAt: *account.LastActivityAt,
		Message:   "近期存在真实请求",
		Source:    "history",
	}
	b.accountResults[account.ID] = result
	b.observations = append(b.observations, result)
	b.passiveAccounts++
}

func (b *cycleBatch) addCachedEvidence(accountID int64, evidence accountEvidence) {
	if evidence.checkedAt == nil || evidence.status == "" {
		return
	}
	b.accountResults[accountID] = model.ProbeResult{
		TargetKey: model.TargetKey(model.KindAccount, accountID),
		Kind:      model.KindAccount,
		EntityID:  accountID,
		Status:    evidence.status,
		CheckedAt: *evidence.checkedAt,
		Source:    "cache",
	}
}

func (b *cycleBatch) addProbeResults(results []model.ProbeResult) {
	for _, result := range results {
		b.persisted = append(b.persisted, result)
		b.observations = append(b.observations, result)
		b.accountResults[result.EntityID] = result
	}
}

func (b *cycleBatch) aggregateGroups(snapshot model.Snapshot, accounts map[int64]model.Account, now time.Time) {
	for _, group := range snapshot.Groups {
		if !group.ProbeEnabled {
			continue
		}
		memberResults := b.groupMemberResults(group, accounts)
		if len(memberResults) == 0 || !containsFreshObservation(memberResults) {
			continue
		}
		// Keep disabled/error accounts out of the aggregation input as well as
		// out of the observed result list. Otherwise stats.AggregateGroup would
		// treat an excluded account as an unknown member.
		aggregationGroup := eligibleGroup(group, accounts)
		result := stats.AggregateGroup(
			model.TargetKey(model.KindGroup, group.ID), aggregationGroup, memberResults, now,
		)
		b.observations = append(b.observations, result)
		if containsProbe(memberResults) {
			b.persisted = append(b.persisted, result)
		}
	}
}

func (b *cycleBatch) groupMemberResults(group model.Group, accounts map[int64]model.Account) []model.ProbeResult {
	results := make([]model.ProbeResult, 0, len(group.AccountIDs))
	for _, accountID := range group.AccountIDs {
		account, exists := accounts[accountID]
		if !exists || account.Status != "active" || !account.Schedulable {
			continue
		}
		if result, exists := b.accountResults[accountID]; exists {
			results = append(results, result)
		}
	}
	return results
}

func eligibleGroup(group model.Group, accounts map[int64]model.Account) model.Group {
	filtered := group
	filtered.AccountIDs = make([]int64, 0, len(group.AccountIDs))
	for _, accountID := range group.AccountIDs {
		account, exists := accounts[accountID]
		if exists && account.Status == "active" && account.Schedulable {
			filtered.AccountIDs = append(filtered.AccountIDs, accountID)
		}
	}
	if len(group.Members) > 0 {
		filtered.Members = make([]model.GroupMember, 0, len(group.Members))
		for _, member := range group.Members {
			account, exists := accounts[member.AccountID]
			if exists && account.Status == "active" && account.Schedulable {
				filtered.Members = append(filtered.Members, member)
			}
		}
	}
	return filtered
}

func (s *Service) evaluateAlerts(ctx context.Context, results []model.ProbeResult, names map[string]string) {
	policy := model.AlertPolicy{
		FailureThreshold:  s.cfg.FailureThreshold,
		RecoveryThreshold: s.cfg.RecoveryThreshold,
	}
	for _, result := range results {
		if err := s.store.EvaluateAlert(ctx, result, names[result.TargetKey], policy); err != nil {
			s.log.Warn("evaluate monitor alert failed", "target", result.TargetKey, "error", err)
		}
	}
}

func (s *Service) maybePrune(ctx context.Context) {
	s.pruneMu.Lock()
	shouldPrune := time.Since(s.lastPrune) >= time.Hour
	if shouldPrune {
		s.lastPrune = time.Now()
	}
	s.pruneMu.Unlock()
	if shouldPrune {
		if err := s.store.Prune(ctx, time.Now().Add(-s.cfg.Retention)); err != nil {
			s.log.Warn("prune monitor history failed", "error", err)
		}
	}
}

func (s *Service) logCycle(snapshot model.Snapshot, probeAccounts []model.Account, batch *cycleBatch) {
	s.log.Info(
		"monitoring cycle completed",
		"queued_accounts", len(probeAccounts),
		"persisted_results", len(batch.persisted),
		"passive_accounts", batch.passiveAccounts,
		"deferred_accounts", batch.deferredAccounts,
		"verified_accounts", batch.verifiedAccounts,
		"observations", len(batch.observations),
		"discovered_targets", len(snapshot.Accounts)+len(snapshot.Groups),
	)
}

func indexAccounts(accounts []model.Account) map[int64]model.Account {
	indexed := make(map[int64]model.Account, len(accounts))
	for _, account := range accounts {
		indexed[account.ID] = account
	}
	return indexed
}

func targetNames(snapshot model.Snapshot) map[string]string {
	names := make(map[string]string, len(snapshot.Accounts)+len(snapshot.Groups))
	for _, account := range snapshot.Accounts {
		names[model.TargetKey(model.KindAccount, account.ID)] = account.Name
	}
	for _, group := range snapshot.Groups {
		names[model.TargetKey(model.KindGroup, group.ID)] = group.Name
	}
	return names
}

func containsProbe(results []model.ProbeResult) bool {
	for _, result := range results {
		if result.Source == "probe" {
			return true
		}
	}
	return false
}

func containsFreshObservation(results []model.ProbeResult) bool {
	for _, result := range results {
		if result.Source == "probe" || result.Source == "history" {
			return true
		}
	}
	return false
}
