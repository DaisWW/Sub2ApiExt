package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DaisWW/Sub2ApiExt/monitoring/internal/monitor"
)

func TestValidTargetKeyRejectsMalformedValues(t *testing.T) {
	for _, key := range []string{"account:1", "group:23", "group:-1"} {
		if !validTargetKey(key) {
			t.Errorf("validTargetKey(%q) = false", key)
		}
	}
	for _, key := range []string{"", "account:", "account:-1", "account:0001", "group:01", "account:group:1", "group:1x", "user:1"} {
		if validTargetKey(key) {
			t.Errorf("validTargetKey(%q) = true", key)
		}
	}
}

func TestMonitoringWebServiceRejectsMutatingMethods(t *testing.T) {
	server := New((*monitor.Service)(nil))
	for _, path := range []string{"/", "/api/v1/monitor/probe", "/api/v1/monitor/activity", "/api/v1/monitor/alerts/1/ack"} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s returned %d, want %d", path, response.Code, http.StatusMethodNotAllowed)
		}
		if allow := response.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("POST %s returned Allow %q", path, allow)
		}
	}
}

func TestRemovedWriteAPIsAreNotExposed(t *testing.T) {
	server := New((*monitor.Service)(nil))
	for _, path := range []string{"/api/v1/monitor/probe", "/api/v1/monitor/alerts/1/ack"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want %d", path, response.Code, http.StatusNotFound)
		}
	}
}

func TestBoundedQueryIntClampsValues(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/?limit=9999", nil)
	if got := boundedQueryInt(request, "limit", 10, 1, 50); got != 50 {
		t.Fatalf("got %d, want max bound", got)
	}
	request = httptest.NewRequest(http.MethodGet, "/?limit=0", nil)
	if got := boundedQueryInt(request, "limit", 10, 1, 50); got != 10 {
		t.Fatalf("got %d, want fallback for non-positive value", got)
	}
}

func TestStaticResponsesSetContentSecurityPolicy(t *testing.T) {
	server := New((*monitor.Service)(nil))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("got status %d", response.Code)
	}
	policy := response.Header().Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("Content-Security-Policy header is missing")
	}
	if !strings.Contains(policy, "frame-ancestors 'self'") || strings.Contains(policy, "frame-ancestors *") {
		t.Fatalf("CSP %q does not restrict iframe embedding to same origin", policy)
	}
	if frameOptions := response.Header().Get("X-Frame-Options"); frameOptions != "SAMEORIGIN" {
		t.Fatalf("got X-Frame-Options %q, want SAMEORIGIN", frameOptions)
	}
	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("got Cache-Control %q, want no-store", cacheControl)
	}
}

func TestUsageShareControlsExcludeUnitCost(t *testing.T) {
	server := New((*monitor.Service)(nil))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("got status %d", response.Code)
	}
	body := response.Body.String()
	for _, kind := range []string{"model", "group"} {
		prefix := `data-share-kind="` + kind + `" data-share-metric=`
		if got := strings.Count(body, prefix); got != 2 {
			t.Errorf("%s share controls = %d, want Tokens and cost only", kind, got)
		}
		if strings.Contains(body, prefix+`"unit_cost"`) {
			t.Errorf("%s share controls still expose unit_cost", kind)
		}
	}
	if !strings.Contains(body, `data-trend-metric="unit_cost"`) {
		t.Error("trend controls lost the unit_cost metric")
	}
}

func TestUsageShareUsesFilledSvgPaths(t *testing.T) {
	server := New((*monitor.Service)(nil))
	request := httptest.NewRequest(http.MethodGet, "/js/usage.js", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("got status %d", response.Code)
	}
	body := response.Body.String()
	for _, marker := range []string{"function donutSlicePath", "<path class=\"donut-slice\"", "fill-rule=\"evenodd\""} {
		if !strings.Contains(body, marker) {
			t.Errorf("usage.js is missing %q", marker)
		}
	}
	if strings.Contains(body, "stroke-dasharray") {
		t.Error("usage.js still relies on dashed circles for donut slices")
	}
}

func TestUsageShareDonutSlicesAreInteractive(t *testing.T) {
	server := New((*monitor.Service)(nil))
	request := httptest.NewRequest(http.MethodGet, "/js/usage.js", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("got status %d", response.Code)
	}
	body := response.Body.String()
	start := strings.Index(body, "function renderDonutSlice")
	if start < 0 {
		t.Fatal("renderDonutSlice implementation is missing")
	}
	end := strings.Index(body[start:], "\n}\n\nfunction donutSlicePath")
	if end < 0 {
		t.Fatal("renderDonutSlice implementation is incomplete")
	}
	slice := body[start : start+end]
	if !strings.Contains(slice, "data-chart-tooltip") || !strings.Contains(slice, "<title>") {
		t.Fatal("donut slices must expose pointer tooltip interaction")
	}
	renderStart := strings.Index(body, "function renderShareDonut")
	if renderStart < 0 {
		t.Fatal("renderShareDonut implementation is missing")
	}
	renderEnd := strings.Index(body[renderStart:], "\n}\n\nfunction shareItems")
	if renderEnd < 0 {
		t.Fatal("renderShareDonut implementation is incomplete")
	}
	render := body[renderStart : renderStart+renderEnd]
	if !strings.Contains(render, "bindChartTooltip(container, '[data-chart-tooltip]'") {
		t.Fatal("share donut tooltip binding must include slices and legend items")
	}
	if !strings.Contains(body, "bindChartTooltip($('#usageTrendChart'") {
		t.Fatal("usage trend tooltip binding was removed")
	}
	stylesRequest := httptest.NewRequest(http.MethodGet, "/styles.css", nil)
	stylesResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(stylesResponse, stylesRequest)
	if stylesResponse.Code != http.StatusOK {
		t.Fatalf("styles.css returned status %d", stylesResponse.Code)
	}
	styles := stylesResponse.Body.String()
	donutRuleStart := strings.Index(styles, ".donut-svg")
	if donutRuleStart < 0 {
		t.Fatal("donut SVG rule is missing")
	}
	donutRuleEnd := strings.Index(styles[donutRuleStart:], "\n")
	if donutRuleEnd < 0 {
		t.Fatal("donut SVG rule is incomplete")
	}
	if strings.Contains(styles[donutRuleStart:donutRuleStart+donutRuleEnd], "pointer-events: none") {
		t.Fatal("donut SVG must receive pointer events")
	}
	if !strings.Contains(styles, ".donut-slice {") || !strings.Contains(styles, ".donut-slice:hover") {
		t.Fatal("donut slice hover styles are missing")
	}
}

func TestEmbeddedToolbarReservesParentControlSpace(t *testing.T) {
	server := New((*monitor.Service)(nil))
	for path, marker := range map[string]string{
		"/app.js":     "document.documentElement.classList.toggle('iframe-embedded', window.self !== window.top);",
		"/styles.css": "html.iframe-embedded .top-actions { margin-right: 96px; }",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d", path, response.Code)
		}
		if !strings.Contains(response.Body.String(), marker) {
			t.Errorf("%s is missing %q", path, marker)
		}
	}
}

func TestDashboardHidesGroupConcurrencyMetric(t *testing.T) {
	server := New((*monitor.Service)(nil))
	request := httptest.NewRequest(http.MethodGet, "/js/dashboard.js", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("got status %d", response.Code)
	}
	body := response.Body.String()
	for _, marker := range []string{
		"const currentConcurrencyMetric = item.kind === 'account'",
		"target-live-group",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js is missing %q", marker)
		}
	}
	if strings.Contains(body, "成员账户并发") {
		t.Error("dashboard.js still exposes group member concurrency")
	}
}

func TestGroupHistoryShowsRequestAccountColumn(t *testing.T) {
	server := New((*monitor.Service)(nil))
	for path, markers := range map[string][]string{
		"/": {
			"id=\"historyAccountHeader\"",
			"经由账户",
		},
		"/js/history-dialog.js": {
			"historyAccountLabel(item, groupHistory)",
			"setAccountColumnVisible(groupHistory)",
			"const availabilityLabel = groupHistory ? '真实请求可用率' : '记录可用率'",
			"分组真实请求记录",
			"账户 #${accountID}",
		},
		"/js/dashboard.js": {
			"this.#openHistory(card.dataset.target, card.dataset.name)",
		},
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d", path, response.Code)
		}
		body := response.Body.String()
		for _, marker := range markers {
			if !strings.Contains(body, marker) {
				t.Errorf("%s is missing %q", path, marker)
			}
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/js/history-dialog.js", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if strings.Contains(response.Body.String(), "currentTarget") || strings.Contains(response.Body.String(), "renderMembers") {
		t.Fatal("history dialog must not render the full current member list")
	}
	if strings.Contains(response.Body.String(), "分组聚合") {
		t.Fatal("history dialog must not render group aggregate rows")
	}
}

func TestDashboardLabelsCurrentHealthWindow(t *testing.T) {
	server := New((*monitor.Service)(nil))
	request := httptest.NewRequest(http.MethodGet, "/js/dashboard.js", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("got status %d", response.Code)
	}
	body := response.Body.String()
	for _, marker := range []string{
		"const availabilityLabel = `近 1 小时通过率${availabilityDetail}`",
		"const currentSample = recentSamples[recentSamples.length - 1]",
		"const availabilityTone = availabilityToneForStatus(currentGridStatus)",
		"function availabilityToneForStatus(status)",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("dashboard.js is missing %q", marker)
		}
	}
}

func TestStaticResponsesUseConfiguredFrameAncestors(t *testing.T) {
	server := New((*monitor.Service)(nil), "'self' https://dashboard.example.test:8443")
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	policy := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "frame-ancestors 'self' https://dashboard.example.test:8443") {
		t.Fatalf("CSP %q does not contain configured frame ancestors", policy)
	}
	if frameOptions := response.Header().Get("X-Frame-Options"); frameOptions != "" {
		t.Fatalf("got X-Frame-Options %q for a multi-origin policy", frameOptions)
	}
}

func TestNewRejectsWildcardFrameAncestors(t *testing.T) {
	server := New((*monitor.Service)(nil), "*")
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if policy := response.Header().Get("Content-Security-Policy"); strings.Contains(policy, "frame-ancestors *") {
		t.Fatalf("wildcard frame ancestor leaked into CSP %q", policy)
	}
}
