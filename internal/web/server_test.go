package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// newTestServer builds a Server with no hosts — enough to exercise the
// dashboard and settings routes without any SSH machinery.
func newTestServer(interval time.Duration, bind string) *Server {
	return NewServer(nil, interval, 8080, bind)
}

func TestDashboardServesHTML(t *testing.T) {
	s := newTestServer(5*time.Second, "127.0.0.1")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	s.handleDashboard(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<html") {
		t.Errorf("dashboard body does not look like HTML")
	}
}

func TestDashboardSecurityHeaders(t *testing.T) {
	s := newTestServer(5*time.Second, "127.0.0.1")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	s.handleDashboard(rec, req)

	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
		t.Errorf("Content-Security-Policy = %q, want default-src 'self'", got)
	}
}

// refreshMS extracts the rendered value, tolerating html/template's whitespace
// padding around the integer.
var refreshMS = regexp.MustCompile(`const REFRESH_MS\s*=\s*(\d+)\s*;`)

func dashboardRefreshMS(t *testing.T, body string) string {
	t.Helper()
	m := refreshMS.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("REFRESH_MS not found in dashboard body")
	}
	return m[1]
}

func TestDashboardRefreshFloor(t *testing.T) {
	// A sub-second collection interval (legal for the CLI) must not collapse
	// the browser refresh to a 0 ms spin against the unauthenticated API.
	s := newTestServer(50*time.Millisecond, "127.0.0.1")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	s.handleDashboard(rec, req)

	if got := dashboardRefreshMS(t, rec.Body.String()); got != "250" {
		t.Errorf("refresh = %s ms, want 250 ms floor", got)
	}
}

func TestDashboardRefreshSeconds(t *testing.T) {
	// Whole-second intervals pass through unchanged.
	s := newTestServer(2*time.Second, "127.0.0.1")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	s.handleDashboard(rec, req)

	if got := dashboardRefreshMS(t, rec.Body.String()); got != "2000" {
		t.Errorf("refresh = %s ms, want 2000", got)
	}
}

func TestDashboardRejectsOtherPaths(t *testing.T) {
	s := newTestServer(5*time.Second, "127.0.0.1")
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	rec := httptest.NewRecorder()

	s.handleDashboard(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for non-root path", rec.Code)
	}
}

func TestSettingsServesJSON(t *testing.T) {
	s := newTestServer(5*time.Second, "127.0.0.1")
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rec := httptest.NewRecorder()

	s.handleSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("settings body is not valid JSON: %v", err)
	}
	// The whole point of /api/settings is that thresholds come from the same
	// source that drives TUI alerting — assert the shape, not the numbers.
	if _, ok := payload["thresholds"]; !ok {
		t.Errorf("settings payload missing thresholds key")
	}
	if _, ok := payload["default_theme"]; !ok {
		t.Errorf("settings payload missing default_theme key")
	}
}

func TestHostsEmptyList(t *testing.T) {
	s := newTestServer(5*time.Second, "127.0.0.1")
	req := httptest.NewRequest(http.MethodGet, "/api/hosts", nil)
	rec := httptest.NewRecorder()

	s.handleHosts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// No hosts configured → still a valid JSON array.
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("hosts body = %q, want []", body)
	}
}
