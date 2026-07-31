package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Yandex OAuth endpoints.
//
// golang.org/x/oauth2/yandex only carries AuthURL and TokenURL, and its device
// flow is not usable here anyway: Config.DeviceAccessToken speaks RFC 8628
// (grant_type=urn:ietf:params:oauth:grant-type:device_code, device_code=...)
// while Yandex expects grant_type=device_code with the code in "code". The
// refresh half is standard, so that part still goes through x/oauth2.
const (
	DeviceCodeURL   = "https://oauth.yandex.ru/device/code"
	TokenURL        = "https://oauth.yandex.ru/token"
	AuthURL         = "https://oauth.yandex.ru/authorize"
	VerificationURL = "https://ya.ru/device"
)

// Scopes needed to read and control the smart home.
var Scopes = []string{"iot:view", "iot:control"}

// OAuth error codes returned while polling for a device token.
const (
	errAuthorizationPending = "authorization_pending"
	errSlowDown             = "slow_down"
	errInvalidGrant         = "invalid_grant"
	errInvalidClient        = "invalid_client"
	errAccessDenied         = "access_denied"
	errExpiredToken         = "expired_token"
	errUnauthorizedClient   = "unauthorized_client"
)

// ErrCodeExpired means the user did not enter the code in time. The caller
// should request a fresh code rather than give up.
var ErrCodeExpired = errors.New("device code expired before the user confirmed it")

// DeviceCode is the response of POST /device/code.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

// URL returns the page the user must open. Yandex returns one, but it has
// changed before, so fall back to the documented address.
func (d DeviceCode) URL() string {
	if d.VerificationURL != "" {
		return d.VerificationURL
	}
	return VerificationURL
}

// oauthError is the RFC 6749 error body.
type oauthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *oauthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Description)
	}
	return e.Code
}

// Flow performs the Yandex device authorization flow.
type Flow struct {
	ClientID     string
	ClientSecret string
	// DeviceID identifies this installation to Yandex. It must be 6-50 ASCII
	// characters and stay stable, so that re-authorizing the same bridge does
	// not accumulate device entries in the user's Yandex account.
	DeviceID   string
	DeviceName string

	HTTPClient    *http.Client
	Logger        *slog.Logger
	DeviceCodeURL string
	TokenURL      string
}

func (f *Flow) httpClient() *http.Client {
	if f.HTTPClient != nil {
		return f.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (f *Flow) logger() *slog.Logger {
	if f.Logger != nil {
		return f.Logger
	}
	return slog.Default()
}

func (f *Flow) deviceCodeURL() string {
	if f.DeviceCodeURL != "" {
		return f.DeviceCodeURL
	}
	return DeviceCodeURL
}

func (f *Flow) tokenURL() string {
	if f.TokenURL != "" {
		return f.TokenURL
	}
	return TokenURL
}

// RequestCode asks Yandex for a device code and the user code to display.
func (f *Flow) RequestCode(ctx context.Context) (*DeviceCode, error) {
	form := url.Values{}
	form.Set("client_id", f.ClientID)
	if f.DeviceID != "" {
		form.Set("device_id", f.DeviceID)
	}
	if f.DeviceName != "" {
		form.Set("device_name", f.DeviceName)
	}
	form.Set("scope", strings.Join(Scopes, " "))

	var dc DeviceCode
	if err := f.postForm(ctx, f.deviceCodeURL(), form, &dc); err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return nil, errors.New("request device code: response is missing device_code or user_code")
	}
	if dc.Interval <= 0 {
		dc.Interval = 5
	}
	if dc.ExpiresIn <= 0 {
		dc.ExpiresIn = 300
	}
	return &dc, nil
}

// Poll exchanges a device code for a token once the user confirms it.
//
// It returns ErrCodeExpired when the code's lifetime (five minutes) runs out,
// which on an unattended Raspberry Pi is the common case — Authorize turns that
// into a fresh code rather than a failure.
func (f *Flow) Poll(ctx context.Context, dc *DeviceCode) (*oauth2.Token, error) {
	// Poll at exactly the advertised interval; if that turns out to be too
	// eager, Yandex answers slow_down and the loop backs off below.
	interval := time.Duration(dc.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)

	form := url.Values{}
	form.Set("grant_type", "device_code")
	form.Set("code", dc.DeviceCode)
	form.Set("client_id", f.ClientID)
	form.Set("client_secret", f.ClientSecret)

	// Wait, attempt, then check the deadline — in that order. Checking the
	// deadline before attempting would mean a code whose lifetime is shorter
	// than one polling interval never gets polled at all.
	for {
		if err := sleepCtx(ctx, interval); err != nil {
			return nil, err
		}

		var resp tokenResponse
		err := f.postForm(ctx, f.tokenURL(), form, &resp)
		if err == nil {
			return resp.token(), nil
		}

		var oerr *oauthError
		if !errors.As(err, &oerr) {
			// A network blip while polling is not fatal; the code is still
			// valid until its deadline.
			f.logger().Warn("device code poll failed, will retry", slog.String("error", err.Error()))
		} else {
			switch oerr.Code {
			case errAuthorizationPending:
				// Keep waiting for the user.
			case errSlowDown:
				interval += 5 * time.Second
				f.logger().Info("Yandex asked us to slow down", slog.Duration("interval", interval))
			case errInvalidGrant, errExpiredToken:
				return nil, ErrCodeExpired
			case errAccessDenied:
				return nil, fmt.Errorf("authorization denied by the user: %w", err)
			case errInvalidClient, errUnauthorizedClient:
				// Wrong credentials or missing scopes: retrying forever would
				// hide a configuration mistake.
				return nil, fmt.Errorf("check YANDEX_CLIENT_ID/YANDEX_CLIENT_SECRET and that the app has iot:view and iot:control: %w", err)
			default:
				return nil, err
			}
		}

		if time.Now().After(deadline) {
			return nil, ErrCodeExpired
		}
	}
}

// Authorize runs the full flow, announcing the code through report and
// requesting a new one whenever the previous expires unconfirmed. It returns
// only on success or when ctx ends.
func (f *Flow) Authorize(ctx context.Context, report func(*DeviceCode)) (*oauth2.Token, error) {
	backoff := 5 * time.Second
	const maxBackoff = 2 * time.Minute

	for {
		dc, err := f.RequestCode(ctx)
		if err != nil {
			// Bad credentials will never start working, so say so and stop
			// rather than retrying forever against a misconfiguration.
			if fatalAuthError(err) {
				return nil, err
			}
			// Anything else is transient — most often a Pi that booted before
			// its network did. Waiting is right; exiting would turn the
			// container's restart policy into a crash loop.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			f.logger().Warn("cannot reach Yandex OAuth, retrying",
				slog.Duration("retry_in", backoff),
				slog.String("error", err.Error()))
			if err := sleepCtx(ctx, backoff); err != nil {
				return nil, err
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = 5 * time.Second

		f.logger().Warn("action required: authorize this bridge with Yandex",
			slog.String("url", dc.URL()),
			slog.String("user_code", dc.UserCode),
			slog.Int("expires_in_seconds", dc.ExpiresIn))
		if report != nil {
			report(dc)
		}

		tok, err := f.Poll(ctx, dc)
		switch {
		case err == nil:
			f.logger().Info("authorized with Yandex")
			return tok, nil
		case errors.Is(err, ErrCodeExpired):
			f.logger().Warn("device code expired unconfirmed, requesting a new one")
			continue
		default:
			return nil, err
		}
	}
}

// fatalAuthError reports whether an OAuth error reflects a misconfiguration
// that no amount of retrying will fix.
func fatalAuthError(err error) bool {
	var oerr *oauthError
	if !errors.As(err, &oerr) {
		return false
	}
	switch oerr.Code {
	case errInvalidClient, errUnauthorizedClient, errAccessDenied:
		return true
	default:
		return false
	}
}

// tokenResponse is the /token success body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func (r tokenResponse) token() *oauth2.Token {
	tokenType := r.TokenType
	if tokenType == "" {
		tokenType = "bearer"
	}
	tok := &oauth2.Token{
		AccessToken:  r.AccessToken,
		TokenType:    tokenType,
		RefreshToken: r.RefreshToken,
	}
	if r.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	}
	return tok
}

// postForm posts a form and decodes the JSON response, turning an OAuth error
// body into an *oauthError regardless of the status code Yandex chose.
func (f *Flow) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := f.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	var oerr oauthError
	if json.Unmarshal(body, &oerr) == nil && oerr.Code != "" {
		return &oerr
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s: %s", endpoint, resp.Status, truncate(string(body), 256))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// DeviceID returns a stable identifier for this installation, generating and
// persisting one on first use. Yandex requires 6-50 ASCII characters.
func DeviceID(path string) (string, error) {
	if data, err := readTrimmed(path); err == nil && len(data) >= 6 {
		return data, nil
	}

	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate device id: %w", err)
	}
	id := "bridge-" + hex.EncodeToString(buf)

	if err := writeFileSync(path, []byte(id+"\n")); err != nil {
		return "", err
	}
	return id, nil
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
