package wait

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// Builders return a derived strategy. They must not touch the receiver so a
// base strategy can be shared by parallel callers.
func TestStrategyBuildersDoNotMutateReceiver(t *testing.T) {
	t.Run("HTTP", func(t *testing.T) {
		base := ForHTTP("/health")
		derived := base.WithPort("8080/tcp").
			WithMethod(http.MethodPost).
			WithHeader("X-Tenant", "acme").
			WithBasicAuth("user", "pass").
			WithTLS().
			WithStartupTimeout(time.Second).
			WithPollInterval(time.Millisecond)

		if base.port != "" || base.portSet || base.method != http.MethodGet ||
			base.username != "" || base.basicAuth || base.useTLS ||
			base.startupTimeout != 0 || base.pollInterval != 0 {
			t.Errorf("base strategy was mutated: %+v", base)
		}
		if len(base.headers) != 0 {
			t.Errorf("base headers were mutated: %v", base.headers)
		}
		if derived.port != "8080/tcp" || !derived.portSet ||
			derived.method != http.MethodPost || derived.headers["X-Tenant"] != "acme" ||
			!derived.basicAuth || !derived.useTLS ||
			derived.startupTimeout != time.Second || derived.pollInterval != time.Millisecond {
			t.Errorf("derived strategy = %+v", derived)
		}
	})

	t.Run("Exec", func(t *testing.T) {
		base := ForExec([]string{"probe"})
		derived := base.WithExitCodeMatcher(func(int) bool { return true }).
			WithStartupTimeout(time.Second).
			WithPollInterval(time.Millisecond).
			WithExecTimeout(time.Second)

		if base.exitMatcher != nil || base.startupTimeout != 0 ||
			base.pollInterval != defaultExecPollInterval || base.execTimeout != 0 {
			t.Errorf("base strategy was mutated: %+v", base)
		}
		if derived.exitMatcher == nil || derived.startupTimeout != time.Second ||
			derived.pollInterval != time.Millisecond || derived.execTimeout != time.Second {
			t.Errorf("derived strategy = %+v", derived)
		}
	})

	t.Run("Log", func(t *testing.T) {
		base := ForLog("ready")
		derived := base.AsRegexp().WithOccurrence(2).
			WithStartupTimeout(time.Second).
			WithPollInterval(time.Millisecond)

		if base.isRegexp || base.occurrences != 1 ||
			base.startupTimeout != 0 || base.pollInterval != 0 {
			t.Errorf("base strategy was mutated: %+v", base)
		}
		if !derived.isRegexp || derived.occurrences != 2 ||
			derived.startupTimeout != time.Second || derived.pollInterval != time.Millisecond {
			t.Errorf("derived strategy = %+v", derived)
		}
	})

	t.Run("Port", func(t *testing.T) {
		base := ForExposedPort()
		derived := base.WithHostOnly().
			WithStartupTimeout(time.Second).
			WithPollInterval(time.Millisecond)

		if base.hostOnly || base.startupTimeout != 0 || base.pollInterval != 0 {
			t.Errorf("base strategy was mutated: %+v", base)
		}
		if !derived.hostOnly || derived.startupTimeout != time.Second || derived.pollInterval != time.Millisecond {
			t.Errorf("derived strategy = %+v", derived)
		}
	})

	t.Run("Healthcheck", func(t *testing.T) {
		base := ForHealthcheck()
		derived := base.WithStartupTimeout(time.Second).WithPollInterval(time.Millisecond)

		if base.startupTimeout != 0 || base.pollInterval != 0 {
			t.Errorf("base strategy was mutated: %+v", base)
		}
		if derived.startupTimeout != time.Second || derived.pollInterval != time.Millisecond {
			t.Errorf("derived strategy = %+v", derived)
		}
	})

	t.Run("Composite", func(t *testing.T) {
		child := ForHTTP("/health")
		all := ForAll(child)
		any := ForAny(child)

		allDerived := all.WithStartupTimeout(time.Second)
		anyDerived := any.WithStartupTimeout(2 * time.Second)

		if all.startupTimeout != 0 || any.startupTimeout != 0 {
			t.Errorf("base composites were mutated: all=%v any=%v", all.startupTimeout, any.startupTimeout)
		}
		if allDerived.startupTimeout != time.Second || anyDerived.startupTimeout != 2*time.Second {
			t.Errorf("derived composites: all=%v any=%v", allDerived.startupTimeout, anyDerived.startupTimeout)
		}
	})
}

// The header map is part of the value: a derived strategy must not alias the
// caller's map or another derived strategy's map.
func TestHTTPWithHeadersCopiesMap(t *testing.T) {
	caller := map[string]string{"X-Tenant": "acme"}
	base := ForHTTP("/health")
	derived := base.WithHeaders(caller)

	caller["X-Other"] = "mutated"
	if _, ok := derived.headers["X-Other"]; ok {
		t.Fatal("derived strategy shares the caller's header map")
	}

	sibling := derived.WithHeader("X-Sibling", "1")
	derived.headers["X-Local"] = "1"
	if _, ok := sibling.headers["X-Local"]; ok {
		t.Fatal("derived strategies share one header map")
	}
	if sibling.headers["X-Tenant"] != "acme" {
		t.Fatalf("sibling headers = %v, want the inherited header", sibling.headers)
	}
}

// The command slice belongs to the strategy: mutating the caller's slice must
// not reconfigure the strategy.
func TestForExecCopiesCommand(t *testing.T) {
	cmd := []string{"probe", "--flag"}
	strategy := ForExec(cmd)
	cmd[0] = "mutated"

	if strategy.cmd[0] != "probe" {
		t.Fatalf("strategy command = %v, want the original", strategy.cmd)
	}
}

// The composite owns its child slice: replacing an element of the caller's
// slice must not change the composite.
func TestForAllAndForAnyCopyStrategySlice(t *testing.T) {
	original := ForHTTP("/health")
	strategies := []Strategy{original}

	all := ForAll(strategies...)
	any := ForAny(strategies...)
	strategies[0] = ForExec([]string{"replaced"})

	if all.strategies[0] != original {
		t.Fatalf("ForAll child = %v, want the original", all.strategies[0])
	}
	if any.strategies[0] != original {
		t.Fatalf("ForAny child = %v, want the original", any.strategies[0])
	}
}

// Parallel callers may derive strategies from one base value. Before builders
// returned copies this raced on the strategy fields and on the header map,
// which could panic with "concurrent map writes".
func TestStrategyBuildersConcurrent(t *testing.T) {
	httpBase := ForHTTP("/health")
	execBase := ForExec([]string{"probe"})
	logBase := ForLog("ready")
	portBase := ForExposedPort()
	healthBase := ForHealthcheck()
	allBase := ForAll(httpBase)
	anyBase := ForAny(httpBase)

	const workers = 32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%d", i)
			timeout := time.Duration(i+1) * time.Millisecond

			httpDerived := httpBase.WithHeader("X-Tenant", tenant).WithStartupTimeout(timeout)
			if httpDerived.headers["X-Tenant"] != tenant || httpDerived.startupTimeout != timeout {
				t.Errorf("HTTP derived strategy = %+v", httpDerived)
			}
			if got := httpBase.WithHeaders(map[string]string{"X-Tenant": tenant}); got.headers["X-Tenant"] != tenant {
				t.Errorf("HTTP WithHeaders = %+v", got)
			}
			if got := execBase.WithStartupTimeout(timeout); got.startupTimeout != timeout {
				t.Errorf("Exec derived strategy = %+v", got)
			}
			if got := logBase.WithOccurrence(i + 1); got.occurrences != i+1 {
				t.Errorf("Log derived strategy = %+v", got)
			}
			if got := portBase.WithStartupTimeout(timeout); got.startupTimeout != timeout {
				t.Errorf("Port derived strategy = %+v", got)
			}
			if got := healthBase.WithPollInterval(timeout); got.pollInterval != timeout {
				t.Errorf("Healthcheck derived strategy = %+v", got)
			}
			if got := allBase.WithStartupTimeout(timeout); got.startupTimeout != timeout {
				t.Errorf("ForAll derived strategy = %+v", got)
			}
			if got := anyBase.WithStartupTimeout(timeout); got.startupTimeout != timeout {
				t.Errorf("ForAny derived strategy = %+v", got)
			}
		}(i)
	}
	wg.Wait()

	if len(httpBase.headers) != 0 {
		t.Errorf("base headers were mutated: %v", httpBase.headers)
	}
	if httpBase.startupTimeout != 0 || execBase.startupTimeout != 0 || logBase.occurrences != 1 ||
		portBase.startupTimeout != 0 || healthBase.pollInterval != 0 ||
		allBase.startupTimeout != 0 || anyBase.startupTimeout != 0 {
		t.Error("a base strategy was mutated by concurrent builders")
	}
}
