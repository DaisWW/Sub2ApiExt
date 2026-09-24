package monitor

import (
	"context"
	"time"
)

func (s *Service) runCostAlerts(ctx context.Context) error {
	// Cost analysis exists to produce the configured notification. Avoid a
	// database scan and state changes while SMTP is intentionally unconfigured.
	if !s.cfg.CostAlerts.Enabled || s.costEmail == nil || !s.costEmail.Enabled() {
		return nil
	}
	events, err := s.store.AnalyzeCostAlerts(ctx, s.cfg.CostAlerts)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	if err := s.costEmail.SendCostAlerts(ctx, events); err != nil {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		discardErr := s.store.DiscardCostAlertNotifications(releaseCtx, events)
		cancel()
		if discardErr != nil {
			s.log.Warn("discard cost alert records after email failure failed", "error", discardErr)
		}
		return err
	}
	if err := s.store.FinalizeCostAlertNotifications(ctx, events); err != nil {
		s.log.Warn("finalize cost alert notifications failed", "error", err)
	}
	s.log.Info("cost anomaly email sent", "events", len(events))
	return nil
}
