package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/model"
)

const (
	openAIOAuthEndpoint    = "https://chatgpt.com/backend-api/codex/responses"
	anthropicOAuthEndpoint = "https://api.anthropic.com/v1/messages?beta=true"
)

type requestSpec struct {
	endpoint string
	payload  any
	headers  map[string]string
}

func buildRequest(ctx context.Context, account model.Account, baseURL, token, modelName string) (*http.Request, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid base URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("base URL must use http or https")
	}
	spec := requestSpecFor(account, baseURL, token, modelName)
	body, err := json.Marshal(spec.payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range spec.headers {
		request.Header.Set(key, value)
	}
	if account.Type == "cookie" {
		request.Header.Set("Cookie", token)
	}
	return request, nil
}

func appendEndpoint(base, endpoint string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return strings.TrimRight(base, "/") + endpoint
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	endpointPath := endpoint
	if strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(endpointPath, "/v1/") {
		endpointPath = strings.TrimPrefix(endpointPath, "/v1")
	}
	parsed.Path = strings.TrimRight(basePath, "/") + endpointPath
	parsed.RawQuery = ""
	return parsed.String()
}
