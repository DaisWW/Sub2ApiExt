package notify

import (
	"context"
	"strings"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/config"
	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

type EmailSender struct {
	config config.EmailConfig
}

func NewEmailSender(cfg config.EmailConfig) *EmailSender {
	return &EmailSender{config: cfg}
}

func (s *EmailSender) Enabled() bool {
	if s == nil {
		return false
	}
	return strings.TrimSpace(s.config.Host) != "" &&
		s.config.Port > 0 &&
		(s.config.Security == "implicit_tls" || s.config.Security == "starttls") &&
		s.config.Timeout > 0 &&
		strings.TrimSpace(s.config.Username) != "" &&
		s.config.Password != "" &&
		strings.TrimSpace(s.config.From) != "" &&
		len(s.config.To) > 0
}

func (s *EmailSender) SendCostAlerts(ctx context.Context, events []model.CostAlertEvent) error {
	if len(events) == 0 || !s.Enabled() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	message, err := buildCostAlertMessage(s.config, events)
	if err != nil {
		return err
	}
	return s.send(ctx, message)
}
