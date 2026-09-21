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

// Limits on the administrator credential.
//
// Passwords carry no length or complexity requirement: the operator decides what
// is strong enough for their deployment. Length is still bounded at a generous
// ceiling so a hostile sign-in request cannot make the gateway hash megabytes,
// and that ceiling is enforced by rejection rather than truncation.
const (
	// MaxUsernameBytes bounds the stored account name.
	MaxUsernameBytes = 64
	// MaxPasswordBytes bounds a password so hashing stays cheap under abuse. It is
	// far above any human password and does not restrict a legitimate one.
	MaxPasswordBytes = 4096

	// DefaultUsername is the account name created when the gateway starts with no
	// administrator, so a fresh deployment is reachable without a CLI step.
	DefaultUsername = "admin"
	// DefaultPassword is the password for that automatically created account. It is
	// not secret, which is exactly why such an account is always created with
	// PasswordChangeRequired set and cannot reach any data before it is changed.
	DefaultPassword = "admin"

	// sessionTokenBytes is the entropy of an issued session token.
	sessionTokenBytes = 32
)

var (
	// ErrInvalidCredentials is returned for both an unknown username and a wrong
	// password so callers cannot distinguish the two.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrPasswordTooLong reports a password above MaxPasswordBytes.
	ErrPasswordTooLong = errors.New("password is too long")
	// ErrUsernameInvalid reports an unusable account name.
	ErrUsernameInvalid = errors.New("username must be non-empty and at most 64 bytes of printable text")
	// ErrPasswordInvalidEncoding reports a password that is not valid UTF-8.
	ErrPasswordInvalidEncoding = errors.New("password must be valid UTF-8")
	// ErrPasswordUnchanged reports a new password equal to the current one, which
	// would leave a default-credential account in place.
	ErrPasswordUnchanged = errors.New("the new password must differ from the current one")
)

// Account is the stored administrator. PasswordHash is a bcrypt hash and is
// never serialized to a client.
type Account struct {
	ID           int64
	Username     string
	PasswordHash string
	// ChangeRequired marks an account holding a well-known default password. It is
	// created true so the console cannot serve data until the operator sets a real
	// password.
	ChangeRequired bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
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

// HashPassword hashes a password for storage.
//
// bcrypt silently ignores input past 72 bytes, so the password is pre-hashed
// with SHA-256 first. That keeps the full entropy of a long passphrase instead of
// quietly accepting one whose tail does not matter, and it keeps the bcrypt
// input a fixed size. The SHA-256 is applied on the wire form only; the hash
// still goes through bcrypt's cost factor, so brute-force resistance is
// unchanged.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	hashed, err := bcrypt.GenerateFromPassword(prehashPassword(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hashed), nil
}

// prehashPassword reduces a password to the fixed 32-byte digest bcrypt accepts
// in full. The encoding step keeps the digest NUL-free, because bcrypt treats a
// NUL as a terminator.
func prehashPassword(password string) []byte {
	sum := sha256.Sum256([]byte(password))
	return []byte(base64.RawStdEncoding.EncodeToString(sum[:]))
}

// ValidatePassword enforces the encoding and ceiling bounds without hashing.
// There is deliberately no minimum length and no complexity rule.
func ValidatePassword(password string) error {
	if len(password) > MaxPasswordBytes {
		return fmt.Errorf("%w: use at most %d bytes", ErrPasswordTooLong, MaxPasswordBytes)
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
	if err := bcrypt.CompareHashAndPassword([]byte(hash), prehashPassword(password)); err != nil {
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
