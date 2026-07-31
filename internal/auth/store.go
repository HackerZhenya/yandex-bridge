// Package auth obtains and maintains the Yandex OAuth token.
//
// Two properties matter more than anything else here, because losing either
// one bricks the bridge until a human intervenes:
//
//   - Yandex rotates the refresh token on every refresh. The new one must be
//     durably on disk before the new access token is used, or a crash at the
//     wrong moment leaves the bridge holding a refresh token the server has
//     already invalidated.
//   - A refresh that fails for a transient reason must not be mistaken for a
//     revoked grant. The first is retried; the second needs the user.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"yandex-bridge/internal/atomicfile"
)

// tokenFileMode keeps the token readable only by the bridge; it is a credential.
const tokenFileMode fs.FileMode = 0o600

// persistedToken is the on-disk form. It is deliberately a local type rather
// than oauth2.Token so that a change in that library cannot alter the file
// format under a deployed bridge.
type persistedToken struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Store persists an OAuth token atomically, keeping the previous version as a
// fallback.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore returns a Store backed by path.
func NewStore(path string) *Store { return &Store{path: path} }

// Path returns the primary token file path.
func (s *Store) Path() string { return s.path }

func (s *Store) backupPath() string { return s.path + ".bak" }

// Load reads the stored token. A missing token is not an error — it just means
// the bridge has never been authorized — and returns (nil, nil).
//
// If the primary file is missing or unreadable the backup is tried, which is
// what makes a crash mid-save survivable.
func (s *Store) Load() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tok, primaryErr := readToken(s.path)
	if primaryErr == nil {
		return tok, nil
	}
	if !errors.Is(primaryErr, os.ErrNotExist) {
		// A corrupt primary file is worth reporting even when the backup
		// saves us, so it does not go unnoticed forever.
		if backup, err := readToken(s.backupPath()); err == nil {
			return backup, nil
		}
		return nil, fmt.Errorf("read token %s: %w", s.path, primaryErr)
	}

	backup, err := readToken(s.backupPath())
	if err == nil {
		return backup, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return nil, fmt.Errorf("read token backup %s: %w", s.backupPath(), err)
}

// Save writes the token atomically, keeping the outgoing one as a backup. A
// crash at any point leaves either the old or the new token fully intact —
// never a truncated file and never an empty one.
func (s *Store) Save(tok *oauth2.Token) error {
	if tok == nil {
		return errors.New("refusing to save a nil token")
	}
	if tok.RefreshToken == "" {
		// Saving a token without a refresh token would silently downgrade the
		// bridge to one that dies when the access token expires.
		return errors.New("refusing to save a token without a refresh token")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(persistedToken{
		AccessToken:  tok.AccessToken,
		TokenType:    tok.TokenType,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
		UpdatedAt:    time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode token: %w", err)
	}
	data = append(data, '\n')

	return atomicfile.WriteWithBackup(s.path, data, tokenFileMode)
}

func readToken(path string) (*oauth2.Token, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p persistedToken
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if p.RefreshToken == "" && p.AccessToken == "" {
		return nil, fmt.Errorf("parse %s: token file has no credentials", path)
	}
	tokenType := p.TokenType
	if tokenType == "" {
		tokenType = "bearer"
	}
	return &oauth2.Token{
		AccessToken:  p.AccessToken,
		TokenType:    tokenType,
		RefreshToken: p.RefreshToken,
		Expiry:       p.Expiry,
	}, nil
}

// writeFileSync writes data to path atomically. Used for the device id, which
// is small, rarely written, and does not warrant a backup copy.
func writeFileSync(path string, data []byte) error {
	return atomicfile.Write(path, data, tokenFileMode)
}
