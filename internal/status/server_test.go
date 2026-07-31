package status

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yandex-bridge/internal/auth"
)

type stubHealth struct {
	healthy  bool
	problems map[string]string
}

func (s stubHealth) Healthy() bool               { return s.healthy }
func (s stubHealth) Problems() map[string]string { return s.problems }

type stubTokens struct{ status auth.Status }

func (s stubTokens) Status() auth.Status { return s.status }

type stubLink struct {
	reachable bool
	failures  int
	lastPoll  time.Time
}

func (s stubLink) Reachable() bool     { return s.reachable }
func (s stubLink) Failures() int       { return s.failures }
func (s stubLink) LastPoll() time.Time { return s.lastPoll }

func newServer(h Health, t Tokens, l Link) *Server {
	return NewServer(":0", h, t, l, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func get(t *testing.T, s *Server) (int, snapshot, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	body := rec.Body.String()
	var snap snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return rec.Code, snap, body
}

func TestHealthyBridgeReturns200(t *testing.T) {
	s := newServer(
		stubHealth{healthy: true},
		stubTokens{status: auth.Status{State: auth.StateOK, Expiry: time.Now().Add(300 * 24 * time.Hour)}},
		stubLink{reachable: true, lastPoll: time.Now()},
	)

	code, snap, _ := get(t, s)
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200", code)
	}
	if snap.Status != "ok" {
		t.Errorf("status field = %q, want ok", snap.Status)
	}
	if !snap.Yandex.Reachable {
		t.Error("yandex.reachable = false")
	}
	if snap.Auth.State != "ok" {
		t.Errorf("auth.state = %q, want ok", snap.Auth.State)
	}
}

// TestDegradedBridgeReturns503 is what makes the Docker healthcheck and any
// external monitor useful: a broken bridge must be visible without parsing.
func TestDegradedBridgeReturns503(t *testing.T) {
	s := newServer(
		stubHealth{healthy: false, problems: map[string]string{"poll": "5 consecutive failed polls"}},
		stubTokens{status: auth.Status{State: auth.StateDegraded, Detail: "refresh failing"}},
		stubLink{reachable: false, failures: 5},
	)

	code, snap, _ := get(t, s)
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	if snap.Status != "degraded" {
		t.Errorf("status field = %q, want degraded", snap.Status)
	}
	if snap.Problems["poll"] == "" {
		t.Error("problems does not include the poll failure")
	}
	if snap.Yandex.ConsecutiveFailures != 5 {
		t.Errorf("consecutive_failures = %d, want 5", snap.Yandex.ConsecutiveFailures)
	}
}

// TestPendingDeviceCodeIsVisible is the point of having this endpoint at all:
// on a headless Pi it is how the user finds the code to enter, without
// trawling container logs.
func TestPendingDeviceCodeIsVisible(t *testing.T) {
	s := newServer(
		stubHealth{healthy: false, problems: map[string]string{"auth": "waiting for confirmation"}},
		stubTokens{status: auth.Status{
			State:       auth.StateReauthRequired,
			Detail:      "waiting for the user to confirm the device code",
			PendingCode: "1234567",
			PendingURL:  "https://ya.ru/device",
		}},
		stubLink{},
	)

	code, snap, body := get(t, s)
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 while unauthorized", code)
	}
	if snap.Auth.UserCode != "1234567" {
		t.Errorf("auth.user_code = %q, want 1234567", snap.Auth.UserCode)
	}
	if snap.Auth.VerificationURL != "https://ya.ru/device" {
		t.Errorf("auth.verification_url = %q", snap.Auth.VerificationURL)
	}
	if !strings.Contains(body, "reauth_required") {
		t.Errorf("body does not report the reauth state: %s", body)
	}
}

func TestNoTokenOrLinkIsTolerated(t *testing.T) {
	// The status server comes up before the rest of the bridge is wired, so
	// it must not panic on missing collaborators.
	s := newServer(stubHealth{healthy: true}, nil, nil)

	code, _, _ := get(t, s)
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200", code)
	}
}

func TestEmptyTimestampsAreOmitted(t *testing.T) {
	s := newServer(
		stubHealth{healthy: true},
		stubTokens{status: auth.Status{State: auth.StateOK}},
		stubLink{reachable: true},
	)

	_, _, body := get(t, s)
	// A zero time rendered as "0001-01-01T00:00:00Z" reads as a real value
	// and would send someone chasing a clock problem.
	if strings.Contains(body, "0001-01-01") {
		t.Errorf("body contains a zero timestamp: %s", body)
	}
}
