package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "token.json"))
}

func TestStoreRoundTrip(t *testing.T) {
	s := testStore(t)

	if tok, err := s.Load(); err != nil || tok != nil {
		t.Fatalf("Load on empty store = %v, %v; want nil, nil", tok, err)
	}

	want := &oauth2.Token{
		AccessToken:  "access-1",
		TokenType:    "bearer",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(365 * 24 * time.Hour).Truncate(time.Second),
	}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
	if !got.Expiry.Equal(want.Expiry) {
		t.Errorf("Expiry = %v, want %v", got.Expiry, want.Expiry)
	}
}

func TestStoreRefusesTokenWithoutRefreshToken(t *testing.T) {
	s := testStore(t)
	// A token with no refresh token would silently become a bridge that dies
	// when the access token expires — exactly the failure being designed out.
	err := s.Save(&oauth2.Token{AccessToken: "access-only"})
	if err == nil {
		t.Fatal("Save accepted a token with no refresh token")
	}
}

func TestStoreIsCredentialOnly(t *testing.T) {
	s := testStore(t)
	if err := s.Save(&oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}
}

func TestStoreKeepsBackupAcrossSaves(t *testing.T) {
	s := testStore(t)

	if err := s.Save(&oauth2.Token{AccessToken: "a1", RefreshToken: "r1"}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := s.Save(&oauth2.Token{AccessToken: "a2", RefreshToken: "r2"}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	backup, err := readToken(s.Path() + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if backup.RefreshToken != "r1" {
		t.Errorf("backup refresh token = %q, want %q", backup.RefreshToken, "r1")
	}
}

func TestStoreFallsBackToBackupWhenPrimaryIsLost(t *testing.T) {
	s := testStore(t)
	if err := s.Save(&oauth2.Token{AccessToken: "a1", RefreshToken: "r1"}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := s.Save(&oauth2.Token{AccessToken: "a2", RefreshToken: "r2"}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	// Simulate the primary file being lost or truncated by an unclean
	// shutdown; the backup must keep the bridge alive.
	if err := os.Remove(s.Path()); err != nil {
		t.Fatalf("remove primary: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || got.RefreshToken != "r1" {
		t.Errorf("Load = %+v, want the backup token r1", got)
	}
}

func TestStoreFallsBackWhenPrimaryIsCorrupt(t *testing.T) {
	s := testStore(t)
	if err := s.Save(&oauth2.Token{AccessToken: "a1", RefreshToken: "r1"}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := s.Save(&oauth2.Token{AccessToken: "a2", RefreshToken: "r2"}); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if err := os.WriteFile(s.Path(), []byte("{ truncated"), 0o600); err != nil {
		t.Fatalf("corrupt primary: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || got.RefreshToken != "r1" {
		t.Errorf("Load = %+v, want the backup token r1", got)
	}
}

// tokenServer is a stand-in for https://oauth.yandex.ru/token.
type tokenServer struct {
	t *testing.T
	// issued counts successful refreshes.
	issued atomic.Int32
	// fail, when set, is returned instead of a token.
	fail func(w http.ResponseWriter)
	// seenRefreshTokens records every refresh_token presented to it.
	seenRefreshTokens []string
	// rotate controls whether a new refresh token is returned each time.
	rotate bool
}

func (ts *tokenServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			ts.t.Errorf("parse form: %v", err)
		}
		if ts.fail != nil {
			ts.fail(w)
			return
		}

		ts.seenRefreshTokens = append(ts.seenRefreshTokens, r.PostForm.Get("refresh_token"))
		n := ts.issued.Add(1)

		refresh := r.PostForm.Get("refresh_token")
		if ts.rotate {
			refresh = "refresh-" + itoa(int(n)+1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-" + itoa(int(n)+1),
			"token_type":    "bearer",
			"expires_in":    31536000,
			"refresh_token": refresh,
		})
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func newSourceWithServer(t *testing.T, ts *tokenServer, seed *oauth2.Token) (*Source, *Store) {
	t.Helper()
	srv := httptest.NewServer(ts.handler())
	t.Cleanup(srv.Close)

	store := testStore(t)
	if seed != nil {
		if err := store.Save(seed); err != nil {
			t.Fatalf("seed store: %v", err)
		}
	}

	src, err := NewSource(Config{
		ClientID:     "client",
		ClientSecret: "secret",
		Store:        store,
		Logger:       discardLogger(),
		TokenURL:     srv.URL,
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	return src, store
}

// TestRotatedRefreshTokenIsPersistedBeforeUse is the regression test for the
// failure mode that kills bridges silently: Yandex hands back a new refresh
// token on every refresh, and if it is not on disk before the new access token
// is used, a restart is left holding a token the server already invalidated.
func TestRotatedRefreshTokenIsPersistedBeforeUse(t *testing.T) {
	ts := &tokenServer{t: t, rotate: true}
	src, store := newSourceWithServer(t, ts, &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(time.Hour), // inside earlyExpiry, so a refresh is due
	})

	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.RefreshToken == "refresh-1" {
		t.Fatal("refresh token was not rotated by the stub server")
	}

	// What is on disk must match what the caller was handed. If these ever
	// diverge, a crash right here loses the account.
	persisted, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if persisted.RefreshToken != tok.RefreshToken {
		t.Errorf("persisted refresh token = %q, in-memory = %q; they must match",
			persisted.RefreshToken, tok.RefreshToken)
	}
	if persisted.AccessToken != tok.AccessToken {
		t.Errorf("persisted access token = %q, in-memory = %q", persisted.AccessToken, tok.AccessToken)
	}
}

func TestRefreshUsesTheRotatedTokenNextTime(t *testing.T) {
	ts := &tokenServer{t: t, rotate: true}
	src, _ := newSourceWithServer(t, ts, &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(time.Hour),
	})

	ctx := context.Background()
	if err := src.ForceRefresh(ctx); err != nil {
		t.Fatalf("first ForceRefresh: %v", err)
	}
	if err := src.ForceRefresh(ctx); err != nil {
		t.Fatalf("second ForceRefresh: %v", err)
	}

	if len(ts.seenRefreshTokens) != 2 {
		t.Fatalf("server saw %d refreshes, want 2", len(ts.seenRefreshTokens))
	}
	if ts.seenRefreshTokens[0] != "refresh-1" {
		t.Errorf("first refresh presented %q, want refresh-1", ts.seenRefreshTokens[0])
	}
	if ts.seenRefreshTokens[1] == "refresh-1" {
		t.Error("second refresh reused the stale refresh token")
	}
}

func TestValidTokenIsReusedWithoutRefreshing(t *testing.T) {
	ts := &tokenServer{t: t, rotate: true}
	src, _ := newSourceWithServer(t, ts, &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(90 * 24 * time.Hour),
	})

	for range 5 {
		if _, err := src.Token(); err != nil {
			t.Fatalf("Token: %v", err)
		}
	}
	if got := ts.issued.Load(); got != 0 {
		t.Errorf("refreshes = %d, want 0 for a comfortably valid token", got)
	}
}

func TestInvalidGrantRequiresReauthorization(t *testing.T) {
	ts := &tokenServer{t: t, fail: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"expired refresh token"}`)
	}}
	src, _ := newSourceWithServer(t, ts, &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(time.Hour),
	})

	_, err := src.Token()
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("Token error = %v, want ErrReauthRequired", err)
	}
	if got := src.Status().State; got != StateReauthRequired {
		t.Errorf("State = %v, want StateReauthRequired", got)
	}
}

// TestTransientRefreshFailureKeepsServingTheCurrentToken guards the other half
// of the classification: a Yandex outage must not be treated as a revoked
// grant, because that would drag the user into a needless re-authorization.
func TestTransientRefreshFailureKeepsServingTheCurrentToken(t *testing.T) {
	ts := &tokenServer{t: t, fail: func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}}
	src, _ := newSourceWithServer(t, ts, &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(time.Hour),
	})

	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "access-1" {
		t.Errorf("AccessToken = %q, want the still-valid access-1", tok.AccessToken)
	}
	status := src.Status()
	if status.State != StateDegraded {
		t.Errorf("State = %v, want StateDegraded", status.State)
	}
	if status.State == StateReauthRequired {
		t.Error("a 503 must never be reported as a revoked grant")
	}
}

func TestNoStoredTokenReportsReauthRequired(t *testing.T) {
	ts := &tokenServer{t: t}
	src, _ := newSourceWithServer(t, ts, nil)

	if src.HasToken() {
		t.Error("HasToken = true for an empty store")
	}
	if got := src.Status().State; got != StateReauthRequired {
		t.Errorf("State = %v, want StateReauthRequired", got)
	}
	if _, err := src.Token(); !errors.Is(err, ErrReauthRequired) {
		t.Errorf("Token error = %v, want ErrReauthRequired", err)
	}
}

// deviceFlowServer stands in for the Yandex device-code endpoints.
type deviceFlowServer struct {
	t *testing.T
	// pendingPolls is how many times /token replies authorization_pending
	// before succeeding.
	pendingPolls atomic.Int32
	codesIssued  atomic.Int32
	// expireFirstCode makes the first code expire unconfirmed.
	expireFirstCode bool
	// expiresIn is the advertised code lifetime in seconds.
	expiresIn int
}

func (d *deviceFlowServer) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/device/code", func(w http.ResponseWriter, r *http.Request) {
		n := d.codesIssued.Add(1)
		expiresIn := d.expiresIn
		if expiresIn == 0 {
			expiresIn = 30
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "device-code-" + itoa(int(n)),
			"user_code":        "1234567",
			"verification_url": "https://ya.ru/device",
			"interval":         1,
			"expires_in":       expiresIn,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if got := r.PostForm.Get("grant_type"); got != "device_code" {
			d.t.Errorf("grant_type = %q, want device_code (Yandex is not RFC 8628)", got)
		}
		if r.PostForm.Get("code") == "" {
			d.t.Error("device code must be sent in \"code\", not \"device_code\"")
		}

		if d.expireFirstCode && strings.HasSuffix(r.PostForm.Get("code"), "-1") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"code expired"}`)
			return
		}
		if d.pendingPolls.Add(-1) >= 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access",
			"token_type":    "bearer",
			"expires_in":    31536000,
			"refresh_token": "fresh-refresh",
		})
	})
	return mux
}

func newFlow(t *testing.T, d *deviceFlowServer) (*Flow, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(d.mux())
	t.Cleanup(srv.Close)
	return &Flow{
		ClientID:      "client",
		ClientSecret:  "secret",
		DeviceID:      "bridge-test",
		DeviceName:    "test",
		Logger:        discardLogger(),
		DeviceCodeURL: srv.URL + "/device/code",
		TokenURL:      srv.URL + "/token",
	}, srv
}

func TestDeviceFlowPollsUntilConfirmed(t *testing.T) {
	d := &deviceFlowServer{t: t}
	d.pendingPolls.Store(2)
	flow, _ := newFlow(t, d)

	var reported *DeviceCode
	tok, err := flow.Authorize(context.Background(), func(dc *DeviceCode) { reported = dc })
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if tok.RefreshToken != "fresh-refresh" {
		t.Errorf("RefreshToken = %q, want fresh-refresh", tok.RefreshToken)
	}
	if tok.Expiry.IsZero() {
		t.Error("Expiry is zero; expires_in was not applied")
	}
	if reported == nil || reported.UserCode != "1234567" {
		t.Errorf("reported code = %+v, want user code 1234567", reported)
	}
}

// TestDeviceFlowRequestsANewCodeWhenTheOldOneExpires matters on an unattended
// Raspberry Pi: Yandex codes live only five minutes, so the common case is
// nobody being there in time.
func TestDeviceFlowRequestsANewCodeWhenTheOldOneExpires(t *testing.T) {
	d := &deviceFlowServer{t: t, expireFirstCode: true}
	flow, _ := newFlow(t, d)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := flow.Authorize(ctx, nil); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got := d.codesIssued.Load(); got < 2 {
		t.Errorf("codes issued = %d, want at least 2 (one expired, one fresh)", got)
	}
}

func TestDeviceFlowRejectsBadClientCredentials(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc", "user_code": "1234567", "interval": 1, "expires_in": 10,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	flow := &Flow{
		ClientID: "wrong", ClientSecret: "wrong", Logger: discardLogger(),
		DeviceCodeURL: srv.URL + "/device/code", TokenURL: srv.URL + "/token",
	}

	// Bad credentials must fail loudly rather than loop forever requesting
	// new codes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := flow.Authorize(ctx, nil)
	if err == nil {
		t.Fatal("Authorize succeeded with invalid_client")
	}
	if !strings.Contains(err.Error(), "iot:view") {
		t.Errorf("error = %v, want a hint about client credentials and scopes", err)
	}
}

func TestDeviceFlowRequestsBothScopes(t *testing.T) {
	var gotScope string
	mux := http.NewServeMux()
	mux.HandleFunc("/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotScope = r.PostForm.Get("scope")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc", "user_code": "1234567", "interval": 1, "expires_in": 10,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	flow := &Flow{ClientID: "c", Logger: discardLogger(), DeviceCodeURL: srv.URL + "/device/code"}
	if _, err := flow.RequestCode(context.Background()); err != nil {
		t.Fatalf("RequestCode: %v", err)
	}
	for _, want := range []string{"iot:view", "iot:control"} {
		if !strings.Contains(gotScope, want) {
			t.Errorf("scope = %q, want it to include %q", gotScope, want)
		}
	}
}

func TestDeviceIDIsStableAcrossCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device_id")

	first, err := DeviceID(path)
	if err != nil {
		t.Fatalf("DeviceID: %v", err)
	}
	second, err := DeviceID(path)
	if err != nil {
		t.Fatalf("DeviceID: %v", err)
	}
	if first != second {
		t.Errorf("DeviceID changed between calls: %q then %q", first, second)
	}
	// Yandex requires 6-50 ASCII characters.
	if len(first) < 6 || len(first) > 50 {
		t.Errorf("DeviceID %q has length %d, want 6..50", first, len(first))
	}
}

func TestAuthorizeStoresTokenAndClearsStatus(t *testing.T) {
	d := &deviceFlowServer{t: t}
	flow, _ := newFlow(t, d)

	store := testStore(t)
	src, err := NewSource(Config{
		ClientID: "client", ClientSecret: "secret",
		Store: store, Flow: flow, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}

	if err := src.EnsureAuthorized(context.Background()); err != nil {
		t.Fatalf("EnsureAuthorized: %v", err)
	}
	if got := src.Status().State; got != StateOK {
		t.Errorf("State = %v, want StateOK", got)
	}

	persisted, err := store.Load()
	if err != nil || persisted == nil {
		t.Fatalf("Load = %v, %v; want the freshly authorized token", persisted, err)
	}
	if persisted.RefreshToken != "fresh-refresh" {
		t.Errorf("persisted refresh token = %q, want fresh-refresh", persisted.RefreshToken)
	}
}

func TestHTTPClientAttachesBearerTokenAndFollowsRefresh(t *testing.T) {
	ts := &tokenServer{t: t, rotate: true}
	src, _ := newSourceWithServer(t, ts, &oauth2.Token{
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
		Expiry:       time.Now().Add(90 * 24 * time.Hour),
	})

	var seen []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{}`)
	}))
	defer api.Close()

	client := src.HTTPClient()
	resp, err := client.Get(api.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()

	if len(seen) != 1 || seen[0] != "Bearer access-1" {
		t.Fatalf("Authorization headers = %v, want [Bearer access-1]", seen)
	}

	// After a forced refresh the very next request must carry the new token;
	// an extra caching layer here would keep sending the revoked one.
	if err := src.ForceRefresh(context.Background()); err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}
	resp, err = client.Get(api.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()

	if len(seen) != 2 || seen[1] == "Bearer access-1" {
		t.Errorf("Authorization headers = %v, want the second to use the refreshed token", seen)
	}
}

func TestPostFormEncodesCredentials(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc", "user_code": "u", "interval": 1, "expires_in": 300,
		})
	}))
	defer srv.Close()

	flow := &Flow{
		ClientID: "cid", ClientSecret: "csecret", DeviceID: "bridge-abc123", DeviceName: "Pi",
		Logger: discardLogger(), DeviceCodeURL: srv.URL,
	}
	if _, err := flow.RequestCode(context.Background()); err != nil {
		t.Fatalf("RequestCode: %v", err)
	}
	if form.Get("client_id") != "cid" {
		t.Errorf("client_id = %q, want cid", form.Get("client_id"))
	}
	if form.Get("device_id") != "bridge-abc123" {
		t.Errorf("device_id = %q, want bridge-abc123", form.Get("device_id"))
	}
	if form.Get("device_name") != "Pi" {
		t.Errorf("device_name = %q, want Pi", form.Get("device_name"))
	}
}
