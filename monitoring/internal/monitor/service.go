package monitor

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/probe"
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
	nextProbe atomic.Int64
}

var errCycleRunning = errors.New("monitoring cycle already running")

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
	if err := s.RunOnce(ctx); err != nil && !errors.Is(err, errCycleRunning) {
		s.log.Error("initial monitoring cycle failed", "error", err)
	}
	ticker := time.NewTicker(s.cfg.Interval)
	defer func() {
		ticker.Stop()
		s.nextProbe.Store(0)
	}()
	s.setNextProbe(time.Now().Add(s.cfg.Interval))
	for {
		select {
		case <-ctx.Done():
			return
		case scheduledAt := <-ticker.C:
			s.setNextProbe(scheduledAt.Add(s.cfg.Interval))
			if err := s.RunOnce(ctx); err != nil && !errors.Is(err, errCycleRunning) {
				s.log.Error("monitoring cycle failed", "error", err)
			}
		}
	}
}

func (s *Service) RunOnce(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errCycleRunning
	}
	defer s.running.Store(false)
	return s.runCycle(ctx)
}

func (s *Service) Dashboard(ctx context.Context) (model.Dashboard, error) {
	dashboard, err := s.store.Dashboard(ctx, s.cfg.Interval*2, int(s.cfg.Interval/time.Second), s.cfg.FailureThreshold)
	if err != nil {
		return model.Dashboard{}, err
	}
	dashboard.ProbeRunning = s.running.Load()
	dashboard.NextProbeAt = s.nextProbeAt()
	return dashboard, nil
}

func (s *Service) LiveActivity(ctx context.Context) (model.LiveActivity, error) {
	return s.store.LiveActivity(ctx)
}

func (s *Service) setNextProbe(value time.Time) {
	s.nextProbe.Store(value.UTC().UnixMilli())
}

func (s *Service) nextProbeAt() *time.Time {
	value := s.nextProbe.Load()
	if value <= 0 {
		return nil
	}
	next := time.UnixMilli(value).UTC()
	return &next
}

func (s *Service) History(ctx context.Context, key string, limit int) ([]model.ProbeResult, error) {
	return s.store.History(ctx, key, limit)
}

func (s *Service) UsageRanking(ctx context.Context, period string, limit int) (model.UsageRanking, error) {
	return s.store.UsageRanking(ctx, period, limit)
}

func (s *Service) Alerts(ctx context.Context, limit int) ([]model.Alert, error) {
	return s.store.Alerts(ctx, limit)
}

func probeEligible(account model.Account) bool {
	return accountIsError(account) || accountIsEnabled(account)
}

func accountIsActive(account model.Account) bool {
	return strings.EqualFold(strings.TrimSpace(account.Status), "active")
}

func accountIsError(account model.Account) bool {
	return strings.EqualFold(strings.TrimSpace(account.Status), "error")
}

func accountIsEnabled(account model.Account) bool {
	return accountIsActive(account) && account.Schedulable
}
