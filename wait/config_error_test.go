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

// Every validation path must return *ConfigError, not only the strategies
// that build one inline. Before this, invalidConfigf returned a bare
// fmt.Errorf, so errors.As could not recover the type from option and HTTP
// validation failures.
func TestInvalidConfigPathsReturnConfigError(t *testing.T) {
	tests := []struct {
		name     string
		strategy string
		field    string
		run      func() error
	}{
		{
			name:     "negative startup timeout",
			strategy: "ForLog",
			field:    "startup timeout",
			run: func() error {
				return Validate(ForLog("ready").WithStartupTimeout(-time.Second))
			},
		},
		{
			name:     "negative poll interval",
			strategy: "ForExec",
			field:    "poll interval",
			run: func() error {
				return Validate(ForExec([]string{"true"}).WithPollInterval(-time.Second))
			},
		},
		{
			name:     "empty HTTP method",
			strategy: "ForHTTP",
			field:    "method",
			run: func() error {
				return Validate(ForHTTP("/").WithMethod(""))
			},
		},
		{
			name:     "HTTP path without leading slash",
			strategy: "ForHTTP",
			field:    "path",
			run: func() error {
				return Validate(ForHTTP("health"))
			},
		},
		{
			name:     "HTTP path with control character",
			strategy: "ForHTTP",
			field:    "path",
			run: func() error {
				return Validate(ForHTTP("/health\x01"))
			},
		},
		{
			name:     "invalid HTTP header name",
			strategy: "ForHTTP",
			field:    "header name",
			run: func() error {
				return Validate(ForHTTP("/").WithHeader("X-Test\n", "value"))
			},
		},
		{
			name:     "invalid HTTP header value",
			strategy: "ForHTTP",
			field:    "header value for X-Test",
			run: func() error {
				return Validate(ForHTTP("/").WithHeader("X-Test", "bad\x01value"))
			},
		},
		{
			name:     "empty log pattern",
			strategy: "ForLog",
			field:    "pattern",
			run: func() error {
				return Validate(ForLog(""))
			},
		},
		{
			name:     "zero log occurrence count",
			strategy: "ForLog",
			field:    "occurrence count",
			run: func() error {
				return Validate(ForLog("ready").WithOccurrence(0))
			},
		},
		{
			name:  "nil strategy",
			field: "strategy",
			run: func() error {
				return Validate(nil)
			},
		},
		{
			name:  "declared port spec",
			field: "port specification",
			run: func() error {
				return ValidateWithPorts(ForListeningPort("6379/tcp"), []string{"not-a-port"})
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
				t.Fatalf("err = %v, want ErrInvalidConfiguration", err)
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("err = %T, want *ConfigError", err)
			}
			if configErr.Strategy != tt.strategy {
				t.Errorf("Strategy = %q, want %q", configErr.Strategy, tt.strategy)
			}
			if configErr.Field != tt.field {
				t.Errorf("Field = %q, want %q", configErr.Field, tt.field)
			}
			if !strings.Contains(err.Error(), configErr.Reason) {
				t.Errorf("err = %q, want reason %q", err, configErr.Reason)
			}
		})
	}
}

// A rejected header value is not echoed: the field names the header and the
// value stays empty because the value itself is redacted configuration.
func TestHTTPHeaderValueErrorKeepsNameInField(t *testing.T) {
	err := Validate(ForHTTP("/").WithHeader("X-Test", "bad\x01value"))
	var configErr *ConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("err = %T, want *ConfigError", err)
	}
	if configErr.Field != "header value for X-Test" {
		t.Errorf("Field = %q, want header value for X-Test", configErr.Field)
	}
	if configErr.Value != "" {
		t.Errorf("Value = %q, want empty for a redacted header value", configErr.Value)
	}
	if strings.Contains(err.Error(), "bad") {
		t.Errorf("err = %q, must not echo the header value", err)
	}
}
