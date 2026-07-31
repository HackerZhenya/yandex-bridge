// Package status serves the bridge's health over plain HTTP, separately from
// the HAP server.
//
// It exists for the cases where HomeKit cannot tell you anything: the container
// healthcheck, an external monitor, and — most usefully — showing the device
// code when the bridge is waiting to be authorized and nobody is watching the
// logs.
package status

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"yandex-bridge/internal/auth"
	"yandex-bridge/internal/bridge"
)

// Health reports the bridge's own view of whether it is working.
type Health interface {
	Healthy() bool
	Problems() map[string]string
}

// Tokens reports the state of the Yandex authorization.
type Tokens interface {
	Status() auth.Status
}

// Link reports the state of the connection to Yandex.
type Link interface {
	Reachable() bool
	Failures() int
	LastPoll() time.Time
}

// Inventory reports what the bridge found in the Yandex account and what it
// mapped each device to.
type Inventory interface {
	Inventory() []bridge.MappingReport
}

// Server serves /healthz, /readyz and /devices.
type Server struct {
	addr      string
	health    Health
	tokens    Tokens
	link      Link
	inventory Inventory
	logger    *slog.Logger
	start     time.Time
}

// NewServer returns a status server bound to addr.
func NewServer(addr string, health Health, tokens Tokens, link Link, inventory Inventory, logger *slog.Logger) *Server {
	return &Server{
		addr:      addr,
		health:    health,
		tokens:    tokens,
		link:      link,
		inventory: inventory,
		logger:    logger,
		start:     time.Now(),
	}
}

// snapshot is the JSON body of /healthz.
type snapshot struct {
	Status   string            `json:"status"`
	Uptime   string            `json:"uptime"`
	Problems map[string]string `json:"problems,omitempty"`
	Auth     authSnapshot      `json:"auth"`
	Yandex   linkSnapshot      `json:"yandex"`
}

type authSnapshot struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	Expiry string `json:"token_expiry,omitempty"`
	// UserCode is shown when the bridge is waiting to be authorized. It is
	// useless to anyone without the owner's Yandex login, and printing it here
	// is what makes a headless re-authorization possible without the logs.
	UserCode        string `json:"user_code,omitempty"`
	VerificationURL string `json:"verification_url,omitempty"`
}

type linkSnapshot struct {
	Reachable           bool   `json:"reachable"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastSuccessfulPoll  string `json:"last_successful_poll,omitempty"`
}

// Run serves until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /devices", s.handleDevices)
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()

	s.logger.Info("status server listening", slog.String("addr", s.addr))

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// handleDevices serves the mapping report.
//
// The device ids needed to write config.yaml are opaque UUIDs that appear
// nowhere in the Home app, so copying them out of a log line is painful. This
// makes it one curl.
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	var reports []bridge.MappingReport
	if s.inventory != nil {
		reports = s.inventory.Inventory()
	}

	body := struct {
		Count   int                    `json:"count"`
		Devices []bridge.MappingReport `json:"devices"`
	}{Count: len(reports), Devices: reports}

	// Device and room names are Cyrillic. JSON is UTF-8 by definition, but a
	// client left to guess may fall back to the system codepage and render
	// them as mojibake, so the encoding is stated explicitly.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if reports == nil {
		// Nothing polled yet: an empty list would look like an empty account.
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		s.logger.Debug("write devices response", slog.String("error", err.Error()))
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snap := snapshot{
		Status:   "ok",
		Uptime:   time.Since(s.start).Round(time.Second).String(),
		Problems: s.health.Problems(),
	}
	if !s.health.Healthy() {
		snap.Status = "degraded"
	}

	if s.tokens != nil {
		st := s.tokens.Status()
		snap.Auth = authSnapshot{
			State:           st.State.String(),
			Detail:          st.Detail,
			UserCode:        st.PendingCode,
			VerificationURL: st.PendingURL,
		}
		if !st.Expiry.IsZero() {
			snap.Auth.Expiry = st.Expiry.UTC().Format(time.RFC3339)
		}
	}

	if s.link != nil {
		snap.Yandex = linkSnapshot{
			Reachable:           s.link.Reachable(),
			ConsecutiveFailures: s.link.Failures(),
		}
		if t := s.link.LastPoll(); !t.IsZero() {
			snap.Yandex.LastSuccessfulPoll = t.UTC().Format(time.RFC3339)
		}
	}

	code := http.StatusOK
	if snap.Status != "ok" {
		// A non-200 lets Docker's healthcheck and any external monitor notice
		// without parsing the body.
		code = http.StatusServiceUnavailable
	}

	// Device and room names are Cyrillic. JSON is UTF-8 by definition, but a
	// client left to guess may fall back to the system codepage and render
	// them as mojibake, so the encoding is stated explicitly.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		s.logger.Debug("write health response", slog.String("error", err.Error()))
	}
}
