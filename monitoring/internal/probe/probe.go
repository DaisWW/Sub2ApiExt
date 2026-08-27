package probe

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"time"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

type Config struct {
	Timeout          time.Duration
	DefaultModel     string
	AllowPrivateHost bool
}

type Prober struct {
	transport    *http.Transport
	timeout      time.Duration
	defaultModel string
	allowPrivate bool
}

type preparationFailure struct {
	class   string
	message string
}

func New(cfg Config) *Prober {
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
		DialContext:         safeDialContext(cfg.AllowPrivateHost),
	}
	return &Prober{
		transport:    transport,
		timeout:      cfg.Timeout,
		defaultModel: cfg.DefaultModel,
		allowPrivate: cfg.AllowPrivateHost,
	}
}

func (p *Prober) Probe(ctx context.Context, account model.Account) model.ProbeResult {
	result := newProbeResult(account)
	if failure := accountBlocker(account); failure != nil {
		return applyPreparationFailure(result, *failure)
	}
	client, request, failure := p.prepareRequest(ctx, account)
	if failure != nil {
		return applyPreparationFailure(result, *failure)
	}
	return p.doRequest(client, request, result)
}

func newProbeResult(account model.Account) model.ProbeResult {
	return model.ProbeResult{
		TargetKey: model.TargetKey(model.KindAccount, account.ID),
		Kind:      model.KindAccount,
		EntityID:  account.ID,
		CheckedAt: time.Now().UTC(),
		Source:    "probe",
	}
}

func accountBlocker(account model.Account) *preparationFailure {
	status := strings.ToLower(strings.TrimSpace(account.Status))
	if status != "error" && (status != "active" || !account.Schedulable) {
		return &preparationFailure{class: "account_disabled", message: "account is not schedulable"}
	}
	if !SupportsAccount(account) {
		return &preparationFailure{class: "unsupported_account", message: "active probe is not supported for this account protocol"}
	}
	if account.ProxyError != "" {
		return &preparationFailure{class: "proxy", message: account.ProxyError}
	}
	return nil
}

func (p *Prober) prepareRequest(ctx context.Context, account model.Account) (*http.Client, *http.Request, *preparationFailure) {
	token := credentialString(account.Credentials, "api_key", "access_token", "token", "key", "session_key")
	if token == "" {
		return nil, nil, &preparationFailure{class: "missing_credential", message: "no supported credential found"}
	}
	baseURL := accountBaseURL(account)
	if err := validateTargetURL(baseURL, true); err != nil {
		return nil, nil, &preparationFailure{class: "configuration", message: err.Error()}
	}
	request, err := buildRequest(ctx, account, baseURL, token, selectModel(account, p.defaultModel))
	if err != nil {
		return nil, nil, &preparationFailure{class: "configuration", message: err.Error()}
	}
	if err := p.validateRequestTarget(ctx, request); err != nil {
		return nil, nil, &preparationFailure{class: "configuration", message: err.Error()}
	}
	client, err := p.clientFor(account.ProxyURL)
	if err != nil {
		return nil, nil, &preparationFailure{class: "proxy", message: err.Error()}
	}
	return client, request, nil
}

func (p *Prober) validateRequestTarget(ctx context.Context, request *http.Request) error {
	if err := validateTargetURL(request.URL.String(), p.allowPrivate); err != nil {
		return err
	}
	return validateResolvedHost(ctx, request.URL.String(), p.allowPrivate)
}

func applyPreparationFailure(result model.ProbeResult, failure preparationFailure) model.ProbeResult {
	if failure.class == "account_disabled" || failure.class == "unsupported_account" {
		result.Status = model.StatusDisabled
	} else {
		result.Status = model.StatusError
	}
	result.ErrorClass = failure.class
	result.Message = strings.TrimSpace(failure.message)
	return result
}
