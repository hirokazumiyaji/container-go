package container

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// flagValues returns every value following flag in args, preserving order.
func flagValues(args []string, flag string) []string {
	var values []string
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			values = append(values, args[i+1])
		}
	}
	return values
}

func TestWithDNSValidation(t *testing.T) {
	cfg := newConfig()
	if err := WithDNS("1.1.1.1", "2001:4860:4860::8888")(cfg); err != nil {
		t.Fatalf("WithDNS: %v", err)
	}
	want := []string{"1.1.1.1", "2001:4860:4860::8888"}
	if !slices.Equal(cfg.dnsServers, want) {
		t.Fatalf("dnsServers = %v, want %v", cfg.dnsServers, want)
	}

	canonical := newConfig()
	if err := WithDNS("0:0:0:0:0:0:0:1")(canonical); err != nil {
		t.Fatalf("WithDNS long IPv6: %v", err)
	}
	if want := []string{"::1"}; !slices.Equal(canonical.dnsServers, want) {
		t.Fatalf("canonical dnsServers = %v, want %v", canonical.dnsServers, want)
	}

	for _, server := range []string{
		"", "not-an-ip", "--dns", "-1.1.1.1", "1.1.1.1/32", "1.1.1.1\n",
		"fe80::1%lo0", " 1.1.1.1", "1.1.1.1 ",
	} {
		t.Run("invalid "+server, func(t *testing.T) {
			assertOptionValidationError(t, WithDNS(server)(newConfig()), "WithDNS")
		})
	}
}

func TestWithStopSignalValidation(t *testing.T) {
	for _, signal := range []string{
		"SIGTERM", "TERM", "SIGKILL", "KILL", "SIGUSR1", "9", "34", "64",
		"SIGRTMIN", "SIGRTMIN+3", "RTMIN+15", "SIGRTMAX-1", "RTMAX-14", "RTMAX",
	} {
		t.Run("valid "+signal, func(t *testing.T) {
			cfg := newConfig()
			if err := WithStopSignal(signal)(cfg); err != nil {
				t.Fatalf("WithStopSignal(%q): %v", signal, err)
			}
			if cfg.stopSignal != signal {
				t.Fatalf("stopSignal = %q, want %q", cfg.stopSignal, signal)
			}
		})
	}
	for _, signal := range []string{
		"", "term", "sigterm", "0", "65", "999", "09", "-9", "--signal",
		"SIGTERM\n", "NOT-A-SIGNAL", "FOO", "SIGTERM+3", "SIGRTMIN+16", "SIGRTMIN+0", "RTMIN+0", "SIGRTMAX-0", "RTMAX-0",
		"RTMIN-3", "SIGRTMAX-15", "SIGTERM;", " SIGTERM", "SIGTERM ",
	} {
		t.Run("invalid "+signal, func(t *testing.T) {
			err := WithStopSignal(signal)(newConfig())
			assertOptionValidationError(t, err, "WithStopSignal")
			if signal != "" && strings.Contains(err.Error(), signal) {
				t.Fatalf("validation error echoed the rejected signal: %v", err)
			}
		})
	}
}

func TestRunOptionValidationPrecedesCLI(t *testing.T) {
	for name, opt := range map[string]Option{
		"dns":         WithDNS("--dns"),
		"stop signal": WithStopSignal("--signal"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newTestRunner()
			_, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), opt, withRunner(f), withEngine(dockerEngine{}))
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("Run error = %v, want ErrInvalidOption", err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("backend was called for an invalid option: %v", f.calls)
			}
		})
	}
}

// Both engines receive the shared run flags; only Docker accepts a
// stop signal, which Apple rejects during its capability check.
func TestRunOptionArgvBuilders(t *testing.T) {
	for _, backend := range []struct {
		name string
		eng  engine
	}{
		{name: "docker", eng: dockerEngine{}},
		{name: "apple", eng: appleEngine{}},
	} {
		t.Run(backend.name, func(t *testing.T) {
			cfg := newConfig()
			cfg.eng = backend.eng
			cfg.name = "myctr"
			for _, opt := range []Option{
				WithReadOnlyRootFS(),
				WithInit(),
				WithDNS("1.1.1.1", "2001:4860:4860::8888"),
			} {
				if err := opt(cfg); err != nil {
					t.Fatalf("apply option: %v", err)
				}
			}
			if backend.name == "docker" {
				if err := WithStopSignal("SIGTERM")(cfg); err != nil {
					t.Fatalf("apply stop signal: %v", err)
				}
			}

			args := backend.eng.runArgs(cfg, "redis:7-alpine", "")
			if !slices.Contains(args, "--read-only") {
				t.Errorf("argv missing --read-only: %v", args)
			}
			if !slices.Contains(args, "--init") {
				t.Errorf("argv missing --init: %v", args)
			}
			wantDNS := []string{"1.1.1.1", "2001:4860:4860::8888"}
			if got := flagValues(args, "--dns"); !slices.Equal(got, wantDNS) {
				t.Errorf("--dns values = %v, want %v (argv %v)", got, wantDNS, args)
			}
			if backend.name == "docker" {
				if got := flagValues(args, "--stop-signal"); !slices.Equal(got, []string{"SIGTERM"}) {
					t.Errorf("--stop-signal values = %v (argv %v)", got, args)
				}
			} else if slices.Contains(args, "--stop-signal") {
				t.Errorf("Apple argv carries an unsupported stop signal: %v", args)
			}
			if args[len(args)-1] != "redis:7-alpine" {
				t.Errorf("image not last: %v", args)
			}
		})
	}
}

func TestAppleRejectsStopSignalBeforeCLI(t *testing.T) {
	err := assertAppleRejectsBeforeCLI(t, WithStopSignal("SIGTERM"))
	if !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("error = %v, want ErrUnsupportedCapability", err)
	}
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %T %v, want *ValidationError", err, err)
	}
	if validationErr.Option != "WithStopSignal" {
		t.Errorf("validation option = %q, want WithStopSignal", validationErr.Option)
	}
}
