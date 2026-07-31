package yandex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// APIError is a non-2xx response from the Yandex API. Unlike a bare status
// string it keeps the body, which is where Yandex puts the human-readable
// reason, and the request id, which is what support asks for.
type APIError struct {
	StatusCode int
	Status     string
	RequestID  string
	Message    string
	Body       string
	// RetryAfter is the parsed Retry-After header on a 429, zero if absent.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "yandex api: %s", e.Status)
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request_id %s)", e.RequestID)
	}
	if e.Message == "" && e.Body != "" {
		fmt.Fprintf(&b, ": %s", truncate(e.Body, 256))
	}
	return b.String()
}

// Unauthorized reports whether the token was rejected. The caller answers this
// by refreshing the token once and retrying, rather than by backing off.
func (e *APIError) Unauthorized() bool { return e.StatusCode == http.StatusUnauthorized }

// Retryable reports whether repeating the request could plausibly succeed.
func (e *APIError) Retryable() bool {
	switch e.StatusCode {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// Device error codes returned in action_result.error_code.
const (
	ErrCodeDeviceUnreachable = "DEVICE_UNREACHABLE"
	ErrCodeDeviceBusy        = "DEVICE_BUSY"
	ErrCodeDeviceNotFound    = "DEVICE_NOT_FOUND"
	ErrCodeInternalError     = "INTERNAL_ERROR"
	ErrCodeInvalidAction     = "INVALID_ACTION"
	ErrCodeInvalidValue      = "INVALID_VALUE"
	ErrCodeNotSupportedMode  = "NOT_SUPPORTED_IN_CURRENT_MODE"
	ErrCodeAccountLinking    = "ACCOUNT_LINKING_ERROR"
	ErrCodeRemoteDisabled    = "REMOTE_CONTROL_DISABLED"
)

// DeviceError is a per-capability failure reported inside an HTTP 200 response
// to POST /v1.0/devices/actions.
type DeviceError struct {
	DeviceID   string
	Capability CapabilityType
	Instance   string
	Code       string
	Message    string
}

func (e *DeviceError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	return fmt.Sprintf("device %s: %s/%s failed: %s", e.DeviceID, e.Capability, e.Instance, msg)
}

// Retryable reports whether repeating the action could plausibly succeed.
// A busy device or a transient cloud error is worth another attempt; an
// invalid value never will be.
func (e *DeviceError) Retryable() bool {
	switch e.Code {
	case ErrCodeDeviceBusy, ErrCodeInternalError:
		return true
	default:
		return false
	}
}

// Unreachable reports whether the device itself could not be contacted. This
// is what turns an accessory "Not Responding" in HomeKit rather than failing
// the whole bridge.
func (e *DeviceError) Unreachable() bool {
	return e.Code == ErrCodeDeviceUnreachable || e.Code == ErrCodeDeviceNotFound
}

// retryabler is implemented by errors that know whether a retry may help.
type retryabler interface{ Retryable() bool }

// Retryable reports whether err is worth retrying. Network and timeout errors
// are retryable by default; anything that explicitly says otherwise is not.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	// A cancelled or expired context means the caller gave up; retrying it
	// would just fail again immediately.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var r retryabler
	if errors.As(err, &r) {
		return r.Retryable()
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// Connection refused, reset and DNS failures surface as *net.OpError;
	// a connection dropped mid-body surfaces as an unexpected EOF.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// Unreachable reports whether err indicates the target device could not be
// reached, as opposed to the bridge failing to talk to Yandex at all.
func Unreachable(err error) bool {
	var de *DeviceError
	return errors.As(err, &de) && de.Unreachable()
}

// joinErrors is errors.Join with a nil result for an empty slice.
func joinErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
