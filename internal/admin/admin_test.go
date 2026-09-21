package admin

import (
	"errors"
	"strings"
	"testing"
)

func TestHashPasswordRejectsOutOfBoundsPasswords(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		password string
		wantErr  error
	}{
		{name: "empty", password: "", wantErr: ErrPasswordTooShort},
		{name: "too short", password: strings.Repeat("a", MinPasswordBytes-1), wantErr: ErrPasswordTooShort},
		{name: "too long", password: strings.Repeat("a", MaxPasswordBytes+1), wantErr: ErrPasswordTooLong},
		{name: "invalid utf8", password: strings.Repeat("a", MinPasswordBytes) + "\xff\xfe", wantErr: ErrPasswordInvalidEncoding},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := HashPassword(testCase.password); !errors.Is(err, testCase.wantErr) {
				t.Fatalf("HashPassword() error = %v, want %v", err, testCase.wantErr)
			}
		})
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
