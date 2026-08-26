package monitor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/probe"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/stats"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/store"
)

type Service struct {
	cfg       config.Config
	store     *store.Store
	prober    *probe.Prober
	log       *slog.Logger
	pruneMu   sync.Mutex
	lastPrune time.Time
	running   atomic.Bool
}

var ErrCycleRunning = errors.New("monitoring cycle already running")

func New(cfg config.Config, repository *store.Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		cfg:   cfg,
		store: repository,
		prober: probe.New(probe.Config{
			Timeout:          cfg.RequestTimeout,
			DefaultModel:     cfg.DefaultModel,
			AllowPrivateHost: cfg.AllowPrivateHost,
		}),
		log: logger,
	}
}

func (s *Service) Run(ctx context.Context) {
	if err := s.RunOnce(ctx); err != nil && !errors.Is(err, ErrCycleRunning) {
		s.log.Error("initial monitoring cycle failed", "error", err)
	}
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RunOnce(ctx); err != nil && !errors.Is(err, ErrCycleRunning) {
				s.log.Error("monitoring cycle failed", "error", err)
			}
		}
	}
}

func (s *Service) RunOnce(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return ErrCycleRunning
	}
	defer s.running.Store(false)
	return s.runCycle(ctx)
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
	accounts := make(map[int64]model.Account, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		accounts[account.ID] = account
	}
	resultsByAccount := make(map[int64]model.ProbeResult)
	observations := make([]model.ProbeResult, 0, len(snapshot.Accounts)+len(snapshot.Groups))
	persistedResults := make([]model.ProbeResult, 0, len(snapshot.Accounts)+len(snapshot.Groups))
	resultCh := make(chan model.ProbeResult, len(snapshot.Accounts))
	sem := make(chan struct{}, s.cfg.ProbeConcurrency)
	var wg sync.WaitGroup
	now := time.Now().UTC()
	passiveAccounts := 0
	for _, account := range snapshot.Accounts {
		if !probeEligible(account) {
			continue
		}
		if account.Status != "error" && account.LastActivityAt != nil && now.Sub(*account.LastActivityAt) < s.cfg.Interval {
			passive := model.ProbeResult{
				TargetKey: model.TargetKey(model.KindAccount, account.ID),
				Kind:      model.KindAccount, EntityID: account.ID,
				Status: model.StatusOperational, CheckedAt: *account.LastActivityAt,
				Message: "recent real request observed", Source: "history",
			}
			resultsByAccount[account.ID] = passive
			observations = append(observations, passive)
			passiveAccounts++
			continue
		}
		account := account
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			resultCh <- s.prober.Probe(ctx, account)
		}()
	}
	go func() {
		wg.Wait()
		close(resultCh)
	}()
	for result := range resultCh {
		persistedResults = append(persistedResults, result)
		observations = append(observations, result)
		resultsByAccount[result.EntityID] = result
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	nameByKey := make(map[string]string, len(snapshot.Accounts)+len(snapshot.Groups))
	for _, account := range snapshot.Accounts {
		nameByKey[model.TargetKey(model.KindAccount, account.ID)] = account.Name
	}
	for _, group := range snapshot.Groups {
		nameByKey[model.TargetKey(model.KindGroup, group.ID)] = group.Name
		if !group.ProbeEnabled {
			continue
		}
		memberResults := make([]model.ProbeResult, 0, len(group.AccountIDs))
		for _, accountID := range group.AccountIDs {
			account, exists := accounts[accountID]
			if !exists || !probeEligible(account) {
				continue
			}
			if result, exists := resultsByAccount[accountID]; exists {
				memberResults = append(memberResults, result)
			}
		}
		if len(memberResults) == 0 {
			continue
		}
		groupResult := stats.AggregateGroup(model.TargetKey(model.KindGroup, group.ID), group, memberResults, time.Now().UTC())
		observations = append(observations, groupResult)
		if containsProbe(memberResults) {
			persistedResults = append(persistedResults, groupResult)
		}
	}
	if err := s.store.InsertResults(ctx, persistedResults); err != nil {
		return err
	}
	policy := model.AlertPolicy{FailureThreshold: s.cfg.FailureThreshold, RecoveryThreshold: s.cfg.RecoveryThreshold}
	for _, result := range observations {
		if err := s.store.EvaluateAlert(ctx, result, nameByKey[result.TargetKey], policy); err != nil {
			s.log.Warn("evaluate monitor alert failed", "target", result.TargetKey, "error", err)
		}
	}
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
	s.log.Info("monitoring cycle completed", "active_probes", len(persistedResults), "passive_accounts", passiveAccounts, "observations", len(observations))
	return nil
}

func (s *Service) Dashboard(ctx context.Context) (model.Dashboard, error) {
	return s.DashboardWindow(ctx, s.cfg.WindowDays)
}

func (s *Service) DashboardWindow(ctx context.Context, days int) (model.Dashboard, error) {
	if days <= 0 || days > 90 {
		days = s.cfg.WindowDays
	}
	dashboard, err := s.store.Dashboard(ctx, days, s.cfg.Interval*2, int(s.cfg.Interval/time.Second))
	dashboard.ProbeRunning = s.running.Load()
	return dashboard, err
}

func (s *Service) History(ctx context.Context, key string, days, limit int) ([]model.ProbeResult, error) {
	return s.store.History(ctx, key, days, limit)
}

func (s *Service) Alerts(ctx context.Context, onlyUnacknowledged bool, limit int) ([]model.Alert, error) {
	return s.store.Alerts(ctx, onlyUnacknowledged, limit)
}

func (s *Service) AcknowledgeAlert(ctx context.Context, id int64) error {
	return s.store.AcknowledgeAlert(ctx, id)
}

func (s *Service) Interval() time.Duration { return s.cfg.Interval }

func (s *Service) Trigger() bool {
	if !s.running.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer s.running.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.runCycle(ctx); err != nil {
			s.log.Error("manual monitoring cycle failed", "error", err)
		}
	}()
	return true
}

func probeEligible(account model.Account) bool {
	return account.Status == "error" || (account.Status == "active" && account.Schedulable)
}

func containsProbe(results []model.ProbeResult) bool {
	for _, result := range results {
		if result.Source == "probe" {
			return true
		}
	}
	return false
}
