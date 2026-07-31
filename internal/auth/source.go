package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// ErrReauthRequired means the refresh token is gone or rejected and only the
// user can fix it, by running the device flow again.
var ErrReauthRequired = errors.New("re-authorization required")

const (
	// earlyExpiry refreshes a day before the token actually expires, turning
	// a hard deadline into a 24-hour window in which retries can succeed.
	earlyExpiry = 24 * time.Hour

	// refreshBefore drives the proactive refresher. Yandex access tokens are
	// long-lived and Yandex itself suggests refreshing every few months, so
	// renewing with a month to spare means expiry is never reached even if
	// the bridge is offline for weeks.
	refreshBefore = 30 * 24 * time.Hour

	// refreshCheckInterval is how often the proactive refresher looks.
	refreshCheckInterval = 6 * time.Hour
)

// State describes what the token source can currently do.
type State int

const (
	// StateOK means a usable token is held.
	StateOK State = iota
	// StateDegraded means refresh is failing for a transient reason but the
	// current access token is still valid.
	StateDegraded
	// StateReauthRequired means the user must authorize the bridge again.
	StateReauthRequired
)

func (s State) String() string {
	switch s {
	case StateOK:
		return "ok"
	case StateDegraded:
		return "degraded"
	case StateReauthRequired:
		return "reauth_required"
	default:
		return "unknown"
	}
}

// Status is a snapshot of the token source for health reporting.
type Status struct {
	State State
	// Detail explains a non-OK state in one line.
	Detail string
	// Expiry is when the current access token expires.
	Expiry time.Time
	// PendingCode is the user code awaiting confirmation, if any. It is safe
	// to display: it is useless without the user's own Yandex login.
	PendingCode string
	// PendingURL is where to enter PendingCode.
	PendingURL string
}

// Healthy reports whether the bridge can currently talk to Yandex.
func (s Status) Healthy() bool { return s.State == StateOK }

// Source is an oauth2.TokenSource that persists every token it obtains and
// distinguishes a transient refresh failure from a revoked grant.
//
// It deliberately does not use oauth2.ReuseTokenSource: that would cache a
// rotated refresh token in memory only, and the whole point here is that the
// rotated token reaches disk before it is used.
type Source struct {
	cfg    *oauth2.Config
	store  *Store
	flow   *Flow
	logger *slog.Logger

	mu      sync.Mutex
	current *oauth2.Token
	status  Status
}

// Config holds what Source needs to construct itself.
type Config struct {
	ClientID     string
	ClientSecret string
	Store        *Store
	Flow         *Flow
	Logger       *slog.Logger
	// TokenURL overrides the OAuth token endpoint. Used by tests.
	TokenURL string
}

// NewSource loads any stored token and returns a Source. A missing token is
// not an error: the caller runs Authorize to obtain one.
func NewSource(cfg Config) (*Source, error) {
	if cfg.Store == nil {
		return nil, errors.New("auth: Store is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = TokenURL
	}

	s := &Source{
		cfg: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Scopes:       Scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:   AuthURL,
				TokenURL:  tokenURL,
				AuthStyle: oauth2.AuthStyleInParams,
			},
		},
		store:  cfg.Store,
		flow:   cfg.Flow,
		logger: logger,
	}

	tok, err := cfg.Store.Load()
	if err != nil {
		return nil, err
	}
	s.current = tok
	if tok == nil {
		s.status = Status{State: StateReauthRequired, Detail: "no stored token; authorization needed"}
	} else {
		s.status = Status{State: StateOK, Expiry: tok.Expiry}
	}
	return s, nil
}

// Status returns a snapshot for health reporting.
func (s *Source) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// HasToken reports whether a token is held, valid or not.
func (s *Source) HasToken() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current != nil
}

// Token implements oauth2.TokenSource. It returns the cached token while it is
// comfortably valid and refreshes otherwise.
func (s *Source) Token() (*oauth2.Token, error) {
	return s.token(context.Background())
}

func (s *Source) token(ctx context.Context) (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.current == nil {
		return nil, ErrReauthRequired
	}
	if s.current.AccessToken != "" && !expiringWithin(s.current, earlyExpiry) {
		return s.current, nil
	}
	return s.refreshLocked(ctx)
}

// ForceRefresh refreshes unconditionally. The Yandex client calls this after a
// 401, which means the token was revoked server-side rather than expired.
func (s *Source) ForceRefresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.current == nil {
		return ErrReauthRequired
	}
	_, err := s.refreshLocked(ctx)
	return err
}

// refreshLocked exchanges the refresh token for a new one. s.mu must be held.
func (s *Source) refreshLocked(ctx context.Context) (*oauth2.Token, error) {
	if s.current == nil || s.current.RefreshToken == "" {
		s.status = Status{State: StateReauthRequired, Detail: "no refresh token stored"}
		return nil, ErrReauthRequired
	}

	// Hand oauth2 a token that is already expired so its ReuseTokenSource
	// cannot decide to skip the refresh.
	stale := &oauth2.Token{
		RefreshToken: s.current.RefreshToken,
		Expiry:       time.Now().Add(-time.Hour),
	}

	fresh, err := s.cfg.TokenSource(ctx, stale).Token()
	if err != nil {
		return s.handleRefreshErrorLocked(err)
	}

	// Yandex rotates the refresh token, but tolerate a response that omits
	// one rather than throwing away the only credential we have.
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = s.current.RefreshToken
	}

	// Persist before publishing. If the process dies between these two lines
	// the bridge restarts with a token it can still refresh; the other order
	// would leave it holding a refresh token Yandex has already rotated away.
	if err := s.store.Save(fresh); err != nil {
		return nil, fmt.Errorf("persist refreshed token: %w", err)
	}

	rotated := fresh.RefreshToken != s.current.RefreshToken
	s.current = fresh
	s.status = Status{State: StateOK, Expiry: fresh.Expiry}
	s.logger.Info("refreshed Yandex token",
		slog.Time("expiry", fresh.Expiry),
		slog.Bool("refresh_token_rotated", rotated))
	return fresh, nil
}

// handleRefreshErrorLocked classifies a refresh failure. s.mu must be held.
func (s *Source) handleRefreshErrorLocked(err error) (*oauth2.Token, error) {
	if fatalGrantError(err) {
		s.status = Status{
			State:  StateReauthRequired,
			Detail: fmt.Sprintf("Yandex rejected the refresh token: %v", err),
		}
		s.logger.Error("refresh token rejected; the bridge must be authorized again",
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("%w: %v", ErrReauthRequired, err)
	}

	// Transient failure. Keep serving the current access token while it is
	// still valid — a Yandex outage should not take the bridge down with it,
	// and refreshing a day early leaves 24 hours to recover in.
	if s.current.AccessToken != "" && time.Now().Before(s.current.Expiry) {
		s.status = Status{
			State:  StateDegraded,
			Detail: fmt.Sprintf("token refresh failing: %v", err),
			Expiry: s.current.Expiry,
		}
		s.logger.Warn("token refresh failed; continuing with the current access token",
			slog.Time("expiry", s.current.Expiry),
			slog.String("error", err.Error()))
		return s.current, nil
	}

	s.status = Status{
		State:  StateDegraded,
		Detail: fmt.Sprintf("token refresh failing and the access token has expired: %v", err),
	}
	return nil, err
}

// fatalGrantError reports whether an OAuth error means the grant is gone for
// good. Anything else — a 500, a timeout, DNS failure — is worth retrying.
func fatalGrantError(err error) bool {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return false
	}
	switch re.ErrorCode {
	case errInvalidGrant, errInvalidClient, errUnauthorizedClient, errAccessDenied:
		return true
	}
	// Some deployments return the code only in the body. A 400 on a refresh
	// with no recognisable code is still the server refusing the grant, while
	// a 5xx is the server being unwell.
	return re.Response != nil && re.Response.StatusCode == http.StatusBadRequest
}

// Authorize runs the device flow and stores the resulting token. It blocks
// until the user confirms the code or ctx ends.
func (s *Source) Authorize(ctx context.Context) error {
	if s.flow == nil {
		return errors.New("auth: no device flow configured")
	}

	report := func(dc *DeviceCode) {
		s.mu.Lock()
		s.status = Status{
			State:       StateReauthRequired,
			Detail:      "waiting for the user to confirm the device code",
			PendingCode: dc.UserCode,
			PendingURL:  dc.URL(),
		}
		s.mu.Unlock()
	}

	tok, err := s.flow.Authorize(ctx, report)
	if err != nil {
		return err
	}
	if tok.RefreshToken == "" {
		return errors.New("Yandex returned no refresh token; check that the OAuth app is not configured for implicit flow")
	}
	if err := s.store.Save(tok); err != nil {
		return fmt.Errorf("persist token: %w", err)
	}

	s.mu.Lock()
	s.current = tok
	s.status = Status{State: StateOK, Expiry: tok.Expiry}
	s.mu.Unlock()
	return nil
}

// EnsureAuthorized obtains a token if none is stored, and verifies that a
// stored one can still be refreshed. It is called once at startup.
func (s *Source) EnsureAuthorized(ctx context.Context) error {
	if !s.HasToken() {
		s.logger.Info("no stored token; starting device authorization")
		return s.Authorize(ctx)
	}
	if _, err := s.token(ctx); err != nil {
		if errors.Is(err, ErrReauthRequired) {
			s.logger.Warn("stored token is no longer usable; starting device authorization")
			return s.Authorize(ctx)
		}
		return err
	}
	return nil
}

// HTTPClient returns a client that attaches the bearer token to every request
// and refreshes it as needed.
//
// The Source is used directly rather than wrapped in oauth2.ReuseTokenSource:
// Source already caches, and a second cache on top would keep handing out the
// old access token after ForceRefresh — defeating the 401 recovery path.
func (s *Source) HTTPClient() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &oauth2.Transport{Source: s, Base: http.DefaultTransport},
	}
}

// RunReauthWatcher re-runs the device flow whenever the stored grant becomes
// unusable, so a revoked or expired refresh token recovers on its own once the
// user enters a code — no restart, and no crash loop in the meantime.
//
// Sending on trigger forces an immediate attempt; the Home app's
// "Re-authenticate" switch uses that.
func (s *Source) RunReauthWatcher(ctx context.Context, trigger <-chan struct{}) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.Status().State != StateReauthRequired {
				continue
			}
		case <-trigger:
			s.logger.Info("re-authorization triggered")
		}

		if err := s.Authorize(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			// Authorize already loops over expired codes, so reaching here
			// means something a retry will not fix — bad credentials, or the
			// user declining. Report it and wait for the next tick rather
			// than spinning.
			s.logger.Error("re-authorization failed", slog.String("error", err.Error()))
			s.mu.Lock()
			s.status = Status{
				State:  StateReauthRequired,
				Detail: fmt.Sprintf("re-authorization failed: %v", err),
			}
			s.mu.Unlock()
		}
	}
}

// RunRefresher renews the token long before it expires, so that expiry is
// reached only if the bridge has been unable to reach Yandex for weeks. It
// returns when ctx is done.
func (s *Source) RunRefresher(ctx context.Context) {
	ticker := time.NewTicker(refreshCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		s.mu.Lock()
		current := s.current
		needsRefresh := current != nil && expiringWithin(current, refreshBefore)
		s.mu.Unlock()

		if current == nil {
			continue
		}
		if !needsRefresh {
			continue
		}

		s.logger.Info("proactively refreshing Yandex token", slog.Time("expiry", current.Expiry))
		if err := s.ForceRefresh(ctx); err != nil {
			// Already logged and classified by refreshLocked; the health
			// accessory picks the failure up through Status.
			s.logger.Warn("proactive refresh failed", slog.String("error", err.Error()))
		}
	}
}

// expiringWithin reports whether tok expires within d. A token with no expiry
// never does — oauth2 uses a zero Expiry to mean "does not expire".
func expiringWithin(tok *oauth2.Token, d time.Duration) bool {
	if tok.Expiry.IsZero() {
		return false
	}
	return time.Now().Add(d).After(tok.Expiry)
}
