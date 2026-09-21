// Package admin holds the single administrator account used to sign in to the
// management console: password hashing, credential verification, and the
// session tokens issued after a successful sign-in.
//
// The console is unauthenticated HTML, so every privileged action is an HTTP
// request the server authenticates independently. A session token is therefore
// a bearer credential, and only its hash is stored so a leaked database cannot
// be replayed as a live session.
package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// Limits on the administrator credential. The upper bounds exist because
// bcrypt silently truncates input beyond 72 bytes; rejecting a longer password
// is safer than accepting one whose tail is ignored.
const (
	// MinPasswordBytes is the shortest accepted password.
	MinPasswordBytes = 12
	// MaxPasswordBytes is bcrypt's hard input limit.
	MaxPasswordBytes = 72
	// MaxUsernameBytes bounds the stored account name.
	MaxUsernameBytes = 64

	// sessionTokenBytes is the entropy of an issued session token.
	sessionTokenBytes = 32
)

var (
	// ErrInvalidCredentials is returned for both an unknown username and a wrong
	// password so callers cannot distinguish the two.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrPasswordTooShort reports a password below MinPasswordBytes.
	ErrPasswordTooShort = errors.New("password is too short")
	// ErrPasswordTooLong reports a password above MaxPasswordBytes.
	ErrPasswordTooLong = errors.New("password is longer than bcrypt accepts")
	// ErrUsernameInvalid reports an unusable account name.
	ErrUsernameInvalid = errors.New("username must be non-empty and at most 64 bytes of printable text")
	// ErrPasswordInvalidEncoding reports a password that is not valid UTF-8.
	ErrPasswordInvalidEncoding = errors.New("password must be valid UTF-8")
)

// Account is the stored administrator. PasswordHash is a bcrypt hash and is
// never serialized to a client.
type Account struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Session is an issued console session. Token is the raw bearer value returned
// to the browser; only TokenHash is persisted.
type Session struct {
	ID        int64
	Token     string
	TokenHash string
	AccountID int64
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// HashPassword validates and hashes a password for storage.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hashed), nil
}

// ValidatePassword enforces the password bounds without hashing.
func ValidatePassword(password string) error {
	if len(password) < MinPasswordBytes {
		return fmt.Errorf("%w: use at least %d bytes", ErrPasswordTooShort, MinPasswordBytes)
	}
	if len(password) > MaxPasswordBytes {
		return fmt.Errorf("%w: bcrypt ignores anything past %d bytes", ErrPasswordTooLong, MaxPasswordBytes)
	}
	if !utf8.ValidString(password) {
		return ErrPasswordInvalidEncoding
	}
	return nil
}

// ValidateUsername normalizes and validates an account name.
func ValidateUsername(username string) (string, error) {
	trimmed := strings.TrimSpace(username)
	if trimmed == "" || len(trimmed) > MaxUsernameBytes || !utf8.ValidString(trimmed) {
		return "", ErrUsernameInvalid
	}
	if strings.ContainsAny(trimmed, "\x00\r\n\t") {
		return "", ErrUsernameInvalid
	}
	return trimmed, nil
}

// VerifyPassword checks a candidate password against a stored bcrypt hash in
// constant time with respect to the hash comparison itself.
func VerifyPassword(hash, password string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return ErrInvalidCredentials
	}
	return nil
}

// NewSessionToken mints a session token and the hash to persist. The raw token
// is returned once and never stored.
func NewSessionToken() (token string, hash string, err error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashSessionToken(token), nil
}

// HashSessionToken derives the stored form of a session token. Tokens are
// already high-entropy random values, so a plain SHA-256 is sufficient and
// keeps lookup a single indexed equality test.
func HashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// EqualTokenHash compares two token hashes in constant time.
func EqualTokenHash(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
