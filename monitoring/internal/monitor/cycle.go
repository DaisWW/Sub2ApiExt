package monitor

import (
	"context"
	"strings"
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

const (
	recoveryProbeInterval     = 15 * time.Minute
	idleRecoveryProbeInterval = 2 * time.Hour
	idleActivityWindow        = 6 * time.Hour
)

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
		recoveryEligible := probeEligible(account)
		evidence := latestAccountEvidence(account)
		if evidence.valid && successfulEvidence(evidence.status) && !successfulEvidenceNeedsRecovery(account, evidence) {
			// A successful real request or probe is sufficient until the next
			// real channel error. Routine freshness checks would spend upstream
			// quota while the account is idle.
			if evidence.source == "history" && shouldObserveHistoryRecovery(account, evidence, now, interval) {
				batch.addPassiveAccount(account)
			} else {
				batch.addCachedEvidence(account.ID, evidence)
			}
			batch.verifiedAccounts++
			continue
		}

		if evidence.valid && (evidence.status == model.StatusFailed || evidence.status == model.StatusError) {
			// Preserve a known failure for group aggregation, but never retry it
			// unless the gateway has reported a channel error (or the account is
			// already in its error state).
			batch.addCachedEvidence(account.ID, evidence)
		}
		if !recoveryEligible || !probe.SupportsAccount(account) {
			continue
		}

		// A new error after successful evidence is the first recovery check for
		// this incident and should run immediately. Once a probe has failed,
		// later error-log rows belong to the same incident and must obey the
		// retry/idle backoff below.
		immediateRecovery := recoveryProbeImmediate(account, evidence, now)
		lastAttempt := account.LastProbeAt
		if evidence.valid && evidence.checkedAt != nil {
			lastAttempt = evidence.checkedAt
		} else if !probeEvidenceStatus(account.LastProbeStatus) {
			// A timestamp without a usable result is not a recovery attempt we
			// should back off from. Let the first error-triggered verification run.
			lastAttempt = nil
		}
		probeInterval := probeRetryDelayForAccount(account, interval, now)
		if !immediateRecovery && lastAttempt != nil && !probeDue(lastAttempt, now, probeInterval) {
			batch.deferredAccounts++
			continue
		}
		probeAccounts = append(probeAccounts, account)
	}
	return batch, probeAccounts
}

func probeRetryInterval(account model.Account, interval time.Duration) time.Duration {
	retry := recoveryProbeInterval
	if configurationFailure(account) {
		retry = 24 * time.Hour
	} else {
		switch {
		case account.ProbeFailureStreak >= 4:
			retry = 24 * time.Hour
		case account.ProbeFailureStreak == 3:
			retry = 6 * time.Hour
		case account.ProbeFailureStreak == 2:
			retry = time.Hour
		}
	}
	return max(interval, retry)
}

// probeRetryDelayForAccount applies a larger floor after a long idle period.
// The first recovery attempt remains immediate; only subsequent retries are
// slowed down when no real traffic has arrived for several hours.
func probeRetryDelayForAccount(account model.Account, interval time.Duration, now time.Time) time.Duration {
	base := probeRetryInterval(account, interval)
	if account.LastProbeAt != nil && accountIdle(account, now) && base < idleRecoveryProbeInterval {
		base = idleRecoveryProbeInterval
	}
	return addProbeJitter(account, base)
}

func addProbeJitter(account model.Account, base time.Duration) time.Duration {
	accountID := account.ID
	if accountID < 0 {
		accountID = -accountID
	}
	jitterPercent := accountID % 11
	return base + time.Duration(jitterPercent)*base/100
}

func accountIdle(account model.Account, now time.Time) bool {
	return account.LastActivityAt == nil || now.Sub(*account.LastActivityAt) >= idleActivityWindow
}

func successfulEvidenceNeedsRecovery(account model.Account, evidence accountEvidence) bool {
	if !probeEligible(account) {
		return false
	}
	return evidence.checkedAt == nil || account.LastChannelErrorAt.After(*evidence.checkedAt)
}

func recoveryProbeImmediate(account model.Account, evidence accountEvidence, now time.Time) bool {
	if !probeEligible(account) {
		return false
	}
	if account.LastChannelErrorAt != nil &&
		(account.LastProbeAt == nil || account.LastChannelErrorAt.After(*account.LastProbeAt)) {
		if account.LastProbeAt == nil {
			return true
		}
		// A failed recovery probe already belongs to this incident. New log
		// rows must not bypass its retry and idle backoff.
		if account.LastProbeStatus == model.StatusFailed || account.LastProbeStatus == model.StatusError {
			return false
		}
		if !probeEvidenceStatus(account.LastProbeStatus) {
			return true
		}
		// A source/configuration update invalidated the old successful probe;
		// the next channel error is therefore a fresh recovery trigger.
		if account.UpdatedAt != nil && account.LastProbeAt.Before(*account.UpdatedAt) &&
			account.UpdatedAt.Sub(*account.LastProbeAt) > model.SourceUpdateActivityGrace {
			return true
		}
		// A new channel error starts recovery promptly unless a probe for the
		// same incident ran within the minimum cooldown.
		return probeDue(account.LastProbeAt, now, recoveryProbeInterval)
	}
	if !evidence.valid || !successfulEvidence(evidence.status) ||
		!successfulEvidenceNeedsRecovery(account, evidence) {
		return false
	}
	if account.LastProbeAt == nil || !probeEvidenceStatus(account.LastProbeStatus) {
		return true
	}
	// A stream of gateway errors is one recovery incident until the minimum
	// cooldown has elapsed. This prevents each new error-log row from forcing an
	// upstream request while still allowing a later incident to be checked
	// promptly.
	return probeDue(account.LastProbeAt, now, recoveryProbeInterval)
}

// probeEligible deliberately has a narrow meaning: an account is probed only
// to recover from a recorded real gateway channel error. An account status of
// error without that evidence remains visible, but never causes an upstream
// request by itself.
func probeEligible(account model.Account) bool {
	if !accountIsEnabled(account) && !strings.EqualFold(strings.TrimSpace(account.Status), "error") {
		return false
	}
	if account.LastChannelErrorAt == nil {
		return false
	}
	if account.UpdatedAt != nil && account.LastChannelErrorAt.Before(*account.UpdatedAt) {
		// A gateway status update can land a few seconds after the error row.
		// Treat that small accounting lag as the same incident, but discard an
		// error that predates a real configuration/source change.
		if account.UpdatedAt.Sub(*account.LastChannelErrorAt) > model.SourceUpdateActivityGrace {
			return false
		}
	}
	if account.LastProbeAt == nil || account.LastChannelErrorAt.After(*account.LastProbeAt) {
		return true
	}
	// Keep retrying an active account only while the recovery probe itself is
	// still failing. A successful probe closes the error-triggered window until
	// the gateway reports another real channel error.
	return account.LastProbeStatus == model.StatusFailed || account.LastProbeStatus == model.StatusError
}

func configurationFailure(account model.Account) bool {
	if account.LastProbeStatusCode != nil && (*account.LastProbeStatusCode == 401 || *account.LastProbeStatusCode == 403) {
		return true
	}
	switch account.LastProbeErrorClass {
	case "configuration", "missing_credential":
		return true
	default:
		return false
	}
}

func latestAccountEvidence(account model.Account) accountEvidence {
	var evidence accountEvidence
	if evidenceTimeValid(account.LastActivityAt, account.UpdatedAt) {
		evidence = accountEvidence{
			status: model.StatusOperational, source: "history", checkedAt: account.LastActivityAt, valid: true,
		}
	}
	if evidenceTimeValid(account.LastProbeAt, account.UpdatedAt) &&
		probeEvidenceStatus(account.LastProbeStatus) &&
		(evidence.checkedAt == nil || account.LastProbeAt.After(*evidence.checkedAt)) {
		evidence = accountEvidence{
			status: account.LastProbeStatus, source: "probe", checkedAt: account.LastProbeAt,
			valid: probeEvidenceStatus(account.LastProbeStatus),
		}
	}
	return evidence
}

func evidenceTimeValid(value, changedAt *time.Time) bool {
	if value == nil || changedAt == nil || !value.Before(*changedAt) {
		return value != nil
	}
	// The gateway can commit updated_at a few seconds after the usage row. Keep
	// that request as valid evidence, while treating larger gaps as a real
	// configuration/source change.
	return changedAt.Sub(*value) <= model.SourceUpdateActivityGrace
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
	if account.LastProbeAt == nil || !evidence.checkedAt.After(*account.LastProbeAt) ||
		(account.LastProbeStatus != model.StatusFailed && account.LastProbeStatus != model.StatusError) {
		return false
	}
	// ProbeFailureStreak is reset by EvaluateAlert after this recovery evidence
	// is consumed. It prevents the same unchanged usage row from generating a
	// recovery observation on every scan cycle.
	if account.ProbeFailureStreak > 0 {
		return true
	}
	// A just-arrived request can be observed before the alert-state write from
	// the preceding probe is visible in the next snapshot.
	return now.Sub(*evidence.checkedAt) < interval
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
		if containsPersistableObservation(memberResults) {
			b.persisted = append(b.persisted, result)
		}
	}
}

func (b *cycleBatch) groupMemberResults(group model.Group, accounts map[int64]model.Account) []model.ProbeResult {
	accountIDs := groupAccountIDs(group)
	results := make([]model.ProbeResult, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		account, exists := accounts[accountID]
		if !exists || !accountIsEnabled(account) {
			continue
		}
		if result, exists := b.accountResults[accountID]; exists {
			results = append(results, result)
		}
	}
	return results
}

func groupAccountIDs(group model.Group) []int64 {
	accountIDs := make([]int64, 0, len(group.Members)+len(group.AccountIDs))
	seen := make(map[int64]struct{}, cap(accountIDs))
	for _, member := range group.Members {
		if member.AccountID <= 0 {
			continue
		}
		if _, exists := seen[member.AccountID]; exists {
			continue
		}
		seen[member.AccountID] = struct{}{}
		accountIDs = append(accountIDs, member.AccountID)
	}
	for _, accountID := range group.AccountIDs {
		if accountID <= 0 {
			continue
		}
		if _, exists := seen[accountID]; exists {
			continue
		}
		seen[accountID] = struct{}{}
		accountIDs = append(accountIDs, accountID)
	}
	return accountIDs
}

func eligibleGroup(group model.Group, accounts map[int64]model.Account) model.Group {
	filtered := group
	filtered.AccountIDs = make([]int64, 0, len(group.Members)+len(group.AccountIDs))
	for _, accountID := range groupAccountIDs(group) {
		account, exists := accounts[accountID]
		if exists && accountIsEnabled(account) {
			filtered.AccountIDs = append(filtered.AccountIDs, accountID)
		}
	}
	if len(group.Members) > 0 {
		filtered.Members = make([]model.GroupMember, 0, len(group.Members))
		for _, member := range group.Members {
			account, exists := accounts[member.AccountID]
			if exists && accountIsEnabled(account) {
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

func containsPersistableObservation(results []model.ProbeResult) bool {
	for _, result := range results {
		if result.Source == "probe" || result.Source == "history" {
			return true
		}
	}
	return false
}

func containsFreshObservation(results []model.ProbeResult) bool {
	return containsPersistableObservation(results)
}
