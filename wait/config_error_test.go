package wait

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Every static configuration error in this package must carry the same
// sentinel, so a caller can detect the whole class with one errors.Is. Before
// this, only ForListeningPort did.
func TestAllConfigErrorsShareSentinel(t *testing.T) {
	tests := []struct {
		name     string
		strategy string
		run      func() error
	}{
		{
			name:     "ForListeningPort udp",
			strategy: "ForListeningPort",
			run: func() error {
				return ForListeningPort("5353/udp").WaitUntilReady(context.Background(), newFakeTarget())
			},
		},
		{
			name:     "ForHTTP udp",
			strategy: "ForHTTP",
			run: func() error {
				return ForHTTP("/").WithPort("5353/udp").
					WithStartupTimeout(50*time.Millisecond).
					WaitUntilReady(context.Background(), newFakeTarget())
			},
		},
		{
			name:     "ForHTTP malformed",
			strategy: "ForHTTP",
			run: func() error {
				return ForHTTP("/").WithPort("not-a-port").
					WithStartupTimeout(50*time.Millisecond).
					WaitUntilReady(context.Background(), newFakeTarget())
			},
		},
		{
			name:     "ForExec empty command",
			strategy: "ForExec",
			run: func() error {
				return ForExec(nil).WaitUntilReady(context.Background(), newFakeTarget())
			},
		},
		{
			name:     "ForLog bad regexp",
			strategy: "ForLog",
			run: func() error {
				return ForLog("[unclosed").AsRegexp().WaitUntilReady(context.Background(), newFakeTarget())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			if err == nil {
				t.Fatal("want a configuration error")
			}
			if !errors.Is(err, ErrInvalidConfiguration) {
				t.Errorf("err = %v, want ErrInvalidConfiguration", err)
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("err = %T, want *ConfigError", err)
			}
			if configErr.Strategy != tt.strategy {
				t.Errorf("Strategy = %q, want %q", configErr.Strategy, tt.strategy)
			}
			// A configuration error must fail immediately, not after the
			// startup budget.
			if strings.Contains(err.Error(), "timed out") {
				t.Errorf("err = %q, want a fail-fast configuration error", err)
			}
		})
	}
}

// The port grammar is shared with the root package through internal/portspec,
// so the two cannot drift. A spec the root parser accepts must be accepted
// here, and one it rejects must be rejected here too.
func TestPortSpecGrammarMatchesSharedParser(t *testing.T) {
	shared := []struct {
		port   string
		accept bool
	}{
		{"6379", true},
		{"6379/tcp", true},
		{"", false},
		{"0", false},
		{"65536", false},
		{"not-a-port", false},
		{"6379/sctp", false},
		{"6379/UDP", false}, // the grammar is case-sensitive
	}
	for _, tt := range shared {
		t.Run(tt.port, func(t *testing.T) {
			err := validateTCPPortSpec("ForListeningPort", tt.port)
			if tt.accept && err != nil {
				t.Errorf("validateTCPPortSpec(%q) = %v, want nil", tt.port, err)
			}
			if !tt.accept && err == nil {
				t.Errorf("validateTCPPortSpec(%q) = nil, want an error", tt.port)
			}
		})
	}
}
