package admin

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Passwords carry no length or complexity requirement beyond an abuse ceiling.
func TestHashPasswordAcceptsAnyReasonablePassword(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		password string
	}{
		{name: "empty", password: ""},
		{name: "single character", password: "a"},
		{name: "short", password: "abc"},
		{name: "the built-in default", password: DefaultPassword},
		{name: "72 bytes", password: strings.Repeat("a", 72)},
		{name: "73 bytes", password: strings.Repeat("a", 73)},
		{name: "100 bytes", password: strings.Repeat("a", 100)},
		{name: "at the ceiling", password: strings.Repeat("a", MaxPasswordBytes)},
		{name: "unicode", password: "密码密码密码"},
		{name: "spaces and symbols", password: "  !@#$%^&*()  "},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			hash, err := HashPassword(testCase.password)
			if err != nil {
				t.Fatalf("HashPassword() error = %v", err)
			}
			if err := VerifyPassword(hash, testCase.password); err != nil {
				t.Fatalf("VerifyPassword() error = %v for the password just hashed", err)
			}
		})
	}
}

func TestHashPasswordRejectsOnlyUnusableInput(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		password string
		wantErr  error
	}{
		{name: "past the ceiling", password: strings.Repeat("a", MaxPasswordBytes+1), wantErr: ErrPasswordTooLong},
		{name: "invalid utf8", password: "valid" + "\xff\xfe", wantErr: ErrPasswordInvalidEncoding},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := HashPassword(testCase.password); !errors.Is(err, testCase.wantErr) {
				t.Fatalf("HashPassword() error = %v, want %v", err, testCase.wantErr)
			}
		})
	}
}

// bcrypt rejects input past 72 bytes outright, so without pre-hashing a long
// passphrase could not be stored at all — and two passwords sharing their first
// 72 bytes would have to stay distinguishable.
func TestLongPasswordsKeepTheirFullEntropy(t *testing.T) {
	prefix := strings.Repeat("x", 72)
	first := prefix + "-first-tail"
	second := prefix + "-second-tail"

	firstHash, err := HashPassword(first)
	if err != nil {
		t.Fatalf("HashPassword() error = %v; long passwords must be storable", err)
	}
	if err := VerifyPassword(firstHash, first); err != nil {
		t.Fatalf("VerifyPassword() error = %v for the stored password", err)
	}
	if err := VerifyPassword(firstHash, second); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("VerifyPassword() error = %v; two passwords sharing a 72-byte prefix must not match", err)
	}
}

// A generated NUL must not truncate the bcrypt input, which is why the prehash
// is base64-encoded rather than used as raw digest bytes.
func TestPrehashHasNoNulBytes(t *testing.T) {
	for i := 0; i < 512; i++ {
		prehashed := prehashPassword(string(rune(i)))
		if bytes.IndexByte(prehashed, 0) != -1 {
			t.Fatalf("prehash of %d contains a NUL byte", i)
		}
	}
}

func TestHashPasswordProducesVerifiableBcryptHash(t *testing.T) {
	const password = "correct-horse-battery-staple"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Fatalf("hash = %q, want a bcrypt prefix", hash)
	}
	if strings.Contains(hash, password) {
		t.Fatal("hash contains the plaintext password")
	}
	if err := VerifyPassword(hash, password); err != nil {
		t.Fatalf("VerifyPassword() error = %v, want nil for the correct password", err)
	}
}

func TestVerifyPasswordRejectsWrongPassword(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	for _, candidate := range []string{"", "wrong", "correct-horse-battery-stapl", "Correct-Horse-Battery-Staple"} {
		if err := VerifyPassword(hash, candidate); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("VerifyPassword(%q) error = %v, want ErrInvalidCredentials", candidate, err)
		}
	}
}

// The same password must not produce the same hash twice, or the database would
// reveal which accounts share a password.
func TestHashPasswordIsSaltedPerCall(t *testing.T) {
	const password = "correct-horse-battery-staple"

	first, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if first == second {
		t.Fatal("two hashes of the same password are identical; the salt is not random")
	}
	if err := VerifyPassword(second, password); err != nil {
		t.Fatalf("VerifyPassword() against the second hash error = %v", err)
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, hash := range []string{"", "not-a-hash", "$2a$10$tooshort"} {
		if err := VerifyPassword(hash, "correct-horse-battery-staple"); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("VerifyPassword(%q) error = %v, want ErrInvalidCredentials", hash, err)
		}
	}
}

func TestValidateUsername(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		username string
		want     string
		wantErr  bool
	}{
		{name: "plain", username: "admin", want: "admin"},
		{name: "trimmed", username: "  admin  ", want: "admin"},
		{name: "unicode", username: "管理员", want: "管理员"},
		{name: "empty", username: "", wantErr: true},
		{name: "whitespace only", username: "   ", wantErr: true},
		{name: "too long", username: strings.Repeat("a", MaxUsernameBytes+1), wantErr: true},
		{name: "newline", username: "ad\nmin", wantErr: true},
		{name: "nul", username: "ad\x00min", wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := ValidateUsername(testCase.username)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("ValidateUsername(%q) error = nil, want an error", testCase.username)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateUsername(%q) error = %v", testCase.username, err)
			}
			if got != testCase.want {
				t.Fatalf("ValidateUsername(%q) = %q, want %q", testCase.username, got, testCase.want)
			}
		})
	}
}

func TestNewSessionTokenIsRandomAndHashed(t *testing.T) {
	token, hash, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken() error = %v", err)
	}
	if len(token) < 40 {
		t.Fatalf("token length = %d, want at least 40 characters of entropy", len(token))
	}
	if hash == token {
		t.Fatal("the stored hash equals the raw token")
	}
	if hash != HashSessionToken(token) {
		t.Fatal("HashSessionToken is not stable for the same token")
	}

	seen := map[string]bool{token: true}
	for i := 0; i < 32; i++ {
		next, _, err := NewSessionToken()
		if err != nil {
			t.Fatalf("NewSessionToken() error = %v", err)
		}
		if seen[next] {
			t.Fatal("NewSessionToken returned a duplicate token")
		}
		seen[next] = true
	}
}

func TestHashSessionTokenSeparatesInputs(t *testing.T) {
	if HashSessionToken("a") == HashSessionToken("b") {
		t.Fatal("two different tokens hash to the same value")
	}
	if HashSessionToken("") != HashSessionToken("") {
		t.Fatal("HashSessionToken is not deterministic")
	}
}
