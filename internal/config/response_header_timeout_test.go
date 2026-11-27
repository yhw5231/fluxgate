package config

import (
	"strings"
	"testing"
	"time"
)

// An explicit 0s must disable the header bound without lifting the request bound.
func TestLoadResponseHeaderTimeout(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", value: ""},
		{name: "disabledwithoutunit", value: "0"},
		{name: "disabledwithunit", value: "0s"},
		{name: "disabledwithwhitespace", value: "\t0s\t"},
		{name: "explicitbound", value: "30s", want: 30 * time.Second},
		{name: "negative", value: "-1s", wantErr: true},
		{name: "invalid", value: "not-a-duration", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("FLUXGATE_RESPONSE_HEADER_TIMEOUT", test.value)
			cfg, err := Load()
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "FLUXGATE_RESPONSE_HEADER_TIMEOUT") {
					t.Fatalf("Load() error = %v, want header-timeout validation error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load()error=%v", err)
			}
			if cfg.ResponseHeaderTimeout != test.want {
				t.Fatalf("ResponseHeaderTimeout=%s,want%s", cfg.ResponseHeaderTimeout, test.want)
			}
			if cfg.RequestTimeout <= 0 {
				t.Fatalf("RequestTimeout=%s,wantpositiveper-attemptbound", cfg.RequestTimeout)
			}
		})
	}
}
