package yandex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxBodySize caps how much of a response is read. The whole smart home comes
// back in one /user/info response, so the limit is generous, but unbounded
// io.ReadAll on a remote endpoint is how a long-running daemon gets OOM-killed
// on a Raspberry Pi.
const maxBodySize = 8 << 20 // 8 MiB

// Refresher forces the next request to use a freshly fetched access token.
// It is satisfied by the auth package's token source.
type Refresher interface {
	ForceRefresh(ctx context.Context) error
}

// Client talks to the Yandex smart home user API.
//
// Authentication is not this type's concern: pass an *http.Client whose
// transport attaches the bearer token (as golang.org/x/oauth2 does). The one
// exception is a 401 on a token we believed to be valid, which means the token
// was revoked server-side rather than expired — see do.
type Client struct {
	httpClient  *http.Client
	baseURL     string
	logger      *slog.Logger
	refresher   Refresher
	userAgent   string
	readPolicy  RetryPolicy
	writePolicy RetryPolicy
}

// Option customises a Client.
type Option func(*Client)

// WithBaseURL overrides the API host. Used by tests.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimSuffix(u, "/") }
}

// WithLogger sets the logger used for retry and error reporting.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) { c.logger = l }
}

// WithRefresher lets the client recover from a revoked access token by forcing
// one token refresh and retrying the request once.
func WithRefresher(r Refresher) Option {
	return func(c *Client) { c.refresher = r }
}

// WithReadPolicy overrides the retry policy used for reads.
func WithReadPolicy(p RetryPolicy) Option {
	return func(c *Client) { c.readPolicy = p }
}

// WithWritePolicy overrides the retry policy used for device actions.
func WithWritePolicy(p RetryPolicy) Option {
	return func(c *Client) { c.writePolicy = p }
}

// New returns a Client. httpClient must attach the Authorization header;
// if it is nil, a default client with a 30s timeout is used and requests will
// fail with 401 until a token-attaching transport is supplied.
func New(httpClient *http.Client, opts ...Option) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	c := &Client{
		httpClient:  httpClient,
		baseURL:     BaseURL,
		logger:      slog.Default(),
		userAgent:   "yandex-bridge/1.0",
		readPolicy:  ReadPolicy,
		writePolicy: WritePolicy,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// UserInfo fetches the entire smart home: rooms, groups, devices, scenarios
// and households. This is the bridge's primary poll, because the user API has
// no push mechanism and this single request covers every device at once.
func (c *Client) UserInfo(ctx context.Context) (*UserInfo, error) {
	var out UserInfo
	err := c.readPolicy.Do(ctx, c.logger, "user_info", func(ctx context.Context) error {
		return c.do(ctx, http.MethodGet, "/v1.0/user/info", nil, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Device fetches the state of a single device. Used for the fast confirmation
// polls right after a write, where re-fetching the whole home would be wasteful.
func (c *Client) Device(ctx context.Context, deviceID string) (*Device, error) {
	if deviceID == "" {
		return nil, fmt.Errorf("device id must not be empty")
	}
	path := "/v1.0/devices/" + url.PathEscape(deviceID)

	var out Device
	err := c.readPolicy.Do(ctx, c.logger, "device", func(ctx context.Context) error {
		return c.do(ctx, http.MethodGet, path, nil, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Act applies actions to devices.
//
// The HTTP call and the device outcomes are retried together: an HTTP 200 with
// action_result ERROR / DEVICE_BUSY is just as transient as a 503, and every
// action this bridge sends is an absolute set rather than a relative change,
// so replaying it is safe.
func (c *Client) Act(ctx context.Context, req ActionRequest) (*ActionResponse, error) {
	if len(req.Devices) == 0 {
		return nil, fmt.Errorf("action request has no devices")
	}

	var out ActionResponse
	err := c.writePolicy.Do(ctx, c.logger, "devices_actions", func(ctx context.Context) error {
		out = ActionResponse{}
		if err := c.do(ctx, http.MethodPost, "/v1.0/devices/actions", req, &out); err != nil {
			return err
		}
		return out.Err()
	})
	if err != nil {
		return &out, err
	}
	return &out, nil
}

// SetCapability is the common case of Act: one capability on one device.
func (c *Client) SetCapability(ctx context.Context, deviceID string, capability CapabilityType, instance string, value any) error {
	_, err := c.Act(ctx, ActionRequest{
		Devices: []DeviceActions{{
			ID: deviceID,
			Actions: []Action{{
				Type:  capability,
				State: ActionState{Instance: instance, Value: value},
			}},
		}},
	})
	return err
}

// do performs one request, decoding a successful response into out.
//
// On a 401 it forces a token refresh and retries once. A 401 here is not an
// expired token — the oauth2 transport refreshes those before the request goes
// out — but a token revoked or invalidated server-side, which only a fresh
// fetch can fix. Retrying more than once would just hammer the token endpoint.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	err := c.doOnce(ctx, method, path, body, out)

	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.Unauthorized() || c.refresher == nil {
		return err
	}

	c.logger.Warn("access token rejected, forcing refresh", slog.String("path", path))
	if refreshErr := c.refresher.ForceRefresh(ctx); refreshErr != nil {
		return fmt.Errorf("token rejected and refresh failed: %w", refreshErr)
	}
	return c.doOnce(ctx, method, path, body, out)
}

func (c *Client) doOnce(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}

	// Yandex reports failures both as non-2xx and, occasionally, as a 200
	// carrying status="error". Decode the envelope first so either shape
	// produces the same error value.
	var env Envelope
	_ = json.Unmarshal(raw, &env)

	if resp.StatusCode != http.StatusOK || (env.Status != "" && !env.OK()) {
		return &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			RequestID:  env.RequestID,
			Message:    env.Message,
			Body:       string(raw),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

// parseRetryAfter understands both forms of the header: delay-seconds and an
// HTTP-date. A malformed value yields zero, letting the backoff decide.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
