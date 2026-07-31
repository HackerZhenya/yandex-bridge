package yandex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fastPolicy keeps retry tests from actually sleeping.
var fastPolicy = RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestClient(t *testing.T, h http.Handler, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	base := []Option{
		WithBaseURL(srv.URL),
		WithLogger(discardLogger()),
		WithReadPolicy(fastPolicy),
		WithWritePolicy(fastPolicy),
	}
	return New(srv.Client(), append(base, opts...)...)
}

func TestUserInfoDecodesFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "user_info.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.0/user/info" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))

	info, err := c.UserInfo(context.Background())
	if err != nil {
		t.Fatalf("UserInfo: %v", err)
	}

	if got, want := len(info.Devices), 3; got != want {
		t.Fatalf("device count = %d, want %d", got, want)
	}
	if got, want := info.RoomName("room-id-1"), "Гостиная"; got != want {
		t.Errorf("RoomName = %q, want %q", got, want)
	}

	lamp := info.Devices[0]
	if got, want := lamp.Type.Base(), DeviceTypeLight; got != want {
		t.Errorf("Type.Base() = %q, want %q", got, want)
	}

	onOff, ok := lamp.Capability(CapabilityOnOff)
	if !ok {
		t.Fatal("lamp has no on_off capability")
	}
	on, err := onOff.OnOffState()
	if err != nil || !on {
		t.Errorf("OnOffState() = %v, %v; want true, nil", on, err)
	}

	brightness, ok := lamp.RangeCapability(RangeBrightness)
	if !ok {
		t.Fatal("lamp has no brightness range")
	}
	params, err := brightness.RangeParameters()
	if err != nil {
		t.Fatalf("RangeParameters: %v", err)
	}
	if params.Range == nil || params.Range.Min != 1 || params.Range.Max != 100 {
		t.Errorf("range = %+v, want 1..100", params.Range)
	}
	if _, value, err := brightness.RangeState(); err != nil || value != 40 {
		t.Errorf("RangeState() = %v, %v; want 40, nil", value, err)
	}

	color, ok := lamp.Capability(CapabilityColorSetting)
	if !ok {
		t.Fatal("lamp has no color_setting capability")
	}
	colorParams, err := color.ColorParameters()
	if err != nil {
		t.Fatalf("ColorParameters: %v", err)
	}
	if !colorParams.SupportsColor() || !colorParams.SupportsTemperature() {
		t.Errorf("colorParams = %+v, want both colour and temperature support", colorParams)
	}
	state, err := color.ColorState()
	if err != nil {
		t.Fatalf("ColorState: %v", err)
	}
	if state.Instance != ColorHSV || state.HSV.H != 210 || state.HSV.S != 80 {
		t.Errorf("ColorState = %+v, want hsv 210/80/60", state)
	}

	// A null room must decode to the empty string, not fail the whole poll.
	climate := info.Devices[2]
	if climate.Room != "" {
		t.Errorf("Room = %q, want empty for null", climate.Room)
	}
	temp, ok := climate.FloatProperty(FloatTemperature)
	if !ok {
		t.Fatal("climate sensor has no temperature property")
	}
	if v, err := temp.FloatState(); err != nil || v != 21.7 {
		t.Errorf("FloatState() = %v, %v; want 21.7, nil", v, err)
	}
	if _, ok := climate.FloatProperty(FloatBatteryLevel); !ok {
		t.Error("climate sensor has no battery_level property")
	}
}

func TestDeviceReportsOfflineState(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"status": "ok",
			"request_id": "r1",
			"id": "lamp-id-1",
			"name": "Лампочка 1",
			"type": "devices.types.light",
			"state": "offline",
			"capabilities": [],
			"properties": []
		}`)
	}))

	dev, err := c.Device(context.Background(), "lamp-id-1")
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	if !dev.State.Offline() {
		t.Errorf("State = %q, want offline", dev.State)
	}
}

func TestActSurfacesDeviceError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"status": "ok",
			"request_id": "r1",
			"devices": [{
				"id": "lamp-id-1",
				"capabilities": [{
					"type": "devices.capabilities.on_off",
					"state": {
						"instance": "on",
						"action_result": {
							"status": "ERROR",
							"error_code": "DEVICE_UNREACHABLE",
							"error_message": "устройство недоступно"
						}
					}
				}]
			}]
		}`)
	}))

	err := c.SetCapability(context.Background(), "lamp-id-1", CapabilityOnOff, "on", true)
	if err == nil {
		t.Fatal("SetCapability succeeded, want DEVICE_UNREACHABLE")
	}
	if !Unreachable(err) {
		t.Errorf("Unreachable(%v) = false, want true", err)
	}

	var de *DeviceError
	if !errors.As(err, &de) {
		t.Fatalf("error is %T, want *DeviceError", err)
	}
	if de.Code != ErrCodeDeviceUnreachable {
		t.Errorf("Code = %q, want %q", de.Code, ErrCodeDeviceUnreachable)
	}
	// An unreachable device is a fact about the device, not a transient
	// glitch: retrying it just delays the "Not Responding" the user needs.
	if Retryable(err) {
		t.Error("DEVICE_UNREACHABLE is retryable, want not retryable")
	}
}

func TestActRetriesBusyDevice(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"status":"ok","devices":[{"id":"d","capabilities":[
				{"type":"devices.capabilities.on_off","state":{"instance":"on",
				 "action_result":{"status":"ERROR","error_code":"DEVICE_BUSY"}}}]}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok","devices":[{"id":"d","capabilities":[
			{"type":"devices.capabilities.on_off","state":{"instance":"on",
			 "action_result":{"status":"DONE"}}}]}]}`)
	}))

	if err := c.SetCapability(context.Background(), "d", CapabilityOnOff, "on", true); err != nil {
		t.Fatalf("SetCapability: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2 (one busy, one success)", got)
	}
}

func TestRetriesServerErrorThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok","devices":[]}`)
	}))

	if _, err := c.UserInfo(context.Background()); err != nil {
		t.Fatalf("UserInfo: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestDoesNotRetryBadRequest(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"status":"error","request_id":"r1","message":"bad request"}`)
	}))

	_, err := c.UserInfo(context.Background())
	if err == nil {
		t.Fatal("UserInfo succeeded, want error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (400 must not be retried)", got)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.RequestID != "r1" || apiErr.Message != "bad request" {
		t.Errorf("APIError = %+v, want request_id and message preserved", apiErr)
	}
}

func TestErrorStatusInsideHTTP200(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Yandex occasionally reports failure with a 200 and status=error.
		_, _ = io.WriteString(w, `{"status":"error","request_id":"r2","message":"nope"}`)
	}))

	if _, err := c.UserInfo(context.Background()); err == nil {
		t.Fatal("UserInfo succeeded, want error for status=error in a 200")
	}
}

func TestRetryAfterHeaderIsHonoured(t *testing.T) {
	var calls atomic.Int32
	start := time.Now()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok","devices":[]}`)
	}), WithReadPolicy(RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Minute}))

	if _, err := c.UserInfo(context.Background()); err != nil {
		t.Fatalf("UserInfo: %v", err)
	}
	// The header asked for a second; the jittered backoff would have been
	// about a millisecond, so a wait near 1s proves the header won.
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %s, want at least ~1s from Retry-After", elapsed)
	}
}

type stubRefresher struct{ calls atomic.Int32 }

func (s *stubRefresher) ForceRefresh(context.Context) error {
	s.calls.Add(1)
	return nil
}

func TestUnauthorizedForcesOneRefreshAndRetries(t *testing.T) {
	var calls atomic.Int32
	refresher := &stubRefresher{}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok","devices":[]}`)
	}), WithRefresher(refresher))

	if _, err := c.UserInfo(context.Background()); err != nil {
		t.Fatalf("UserInfo: %v", err)
	}
	if got := refresher.calls.Load(); got != 1 {
		t.Errorf("ForceRefresh calls = %d, want 1", got)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("http calls = %d, want 2", got)
	}
}

func TestUnauthorizedRefreshesOnlyOnce(t *testing.T) {
	var calls atomic.Int32
	refresher := &stubRefresher{}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}), WithRefresher(refresher))

	if _, err := c.UserInfo(context.Background()); err == nil {
		t.Fatal("UserInfo succeeded, want error")
	}
	// A permanently rejected token must not turn into a refresh loop against
	// the OAuth endpoint.
	if got := refresher.calls.Load(); got != 1 {
		t.Errorf("ForceRefresh calls = %d, want exactly 1", got)
	}
}

func TestActSendsAbsoluteValues(t *testing.T) {
	var got ActionRequest
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = io.WriteString(w, `{"status":"ok","devices":[]}`)
	}))

	if err := c.SetCapability(context.Background(), "lamp", CapabilityRange, "brightness", 40); err != nil {
		t.Fatalf("SetCapability: %v", err)
	}
	if len(got.Devices) != 1 || got.Devices[0].ID != "lamp" {
		t.Fatalf("request = %+v, want one device \"lamp\"", got)
	}
	action := got.Devices[0].Actions[0]
	if action.Type != CapabilityRange || action.State.Instance != "brightness" {
		t.Errorf("action = %+v, want range/brightness", action)
	}
	if action.State.Value != float64(40) {
		t.Errorf("value = %v, want 40", action.State.Value)
	}
}

func TestContextCancellationStopsRetries(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}), WithReadPolicy(RetryPolicy{MaxAttempts: 100, BaseDelay: 50 * time.Millisecond, MaxDelay: time.Second}))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	if _, err := c.UserInfo(ctx); err == nil {
		t.Fatal("UserInfo succeeded, want error")
	}
	if got := calls.Load(); got > 5 {
		t.Errorf("calls = %d, want the retry loop to stop when the context expired", got)
	}
}
