package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type adminAPI struct {
	baseURL string
	client  *http.Client
}

type adminAPIResponseError struct {
	statusCode int
	code       int
}

func (e *adminAPIResponseError) Error() string {
	if e.statusCode != 0 {
		return fmt.Sprintf("update account priority returned HTTP %d", e.statusCode)
	}
	return fmt.Sprintf("update account priority failed: code=%d", e.code)
}

func isAdminAccountNotFound(err error) bool {
	var responseError *adminAPIResponseError
	return errors.As(err, &responseError) &&
		(responseError.statusCode == http.StatusNotFound || responseError.code == http.StatusNotFound)
}

func newAdminAPI(baseURL string, client *http.Client) *adminAPI {
	if client == nil {
		client = &http.Client{}
	}
	return &adminAPI{baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

func (a *adminAPI) updatePriority(ctx context.Context, apiKey string, accountID int64, priority int) error {
	if accountID <= 0 {
		return fmt.Errorf("account id must be positive")
	}
	if priority <= 0 || priority > 10000 {
		return fmt.Errorf("priority must be between 1 and 10000")
	}
	body, err := json.Marshal(struct {
		Priority int `json:"priority"`
	}{Priority: priority})
	if err != nil {
		return fmt.Errorf("encode account priority update: %w", err)
	}
	endpoint := a.baseURL + "/api/v1/admin/accounts/" + strconv.FormatInt(accountID, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create account priority update: %w", err)
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sub2api-priority-sync/0.1.0")
	response, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("update account priority: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &adminAPIResponseError{statusCode: response.StatusCode}
	}
	var envelope struct {
		Code int `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&envelope); err != nil {
		return fmt.Errorf("decode account priority response: %w", err)
	}
	if envelope.Code != 0 {
		return &adminAPIResponseError{code: envelope.Code}
	}
	return nil
}
