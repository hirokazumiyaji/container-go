package container

import (
	"net/netip"
	"strings"
	"sync"
	"testing"
)

func TestContainerDiagnosticRedactorIsCachedWithoutExtraValues(t *testing.T) {
	ctr := &Container{
		id:        "container-identity",
		exposed:   []portSpec{{Port: 6379, Proto: "tcp"}},
		published: []publishSpec{{HostAddr: netip.MustParseAddr("127.0.0.1"), HostPort: 16379, ContainerPort: 6379, Proto: "tcp", Raw: "127.0.0.1:16379:6379/tcp"}},
		diagnosticRedactorValue: newDiagnosticMatcher([]string{
			"plain-base-value", "127.0.0.1:16379:6379/tcp",
		}),
	}

	first := ctr.diagnosticRedactor()
	if cached := ctr.diagnosticRedactor(); cached != first {
		t.Fatal("handle matcher was rebuilt despite no per-call values")
	}
	withExtra := ctr.diagnosticRedactor("plain-extra-value")
	if withExtra == first {
		t.Fatal("per-call values reused the cached matcher")
	}
	if got := withExtra.Text("plain-extra-value"); strings.Contains(got, "plain-extra-value") {
		t.Fatalf("per-call value was not redacted: %q", got)
	}

	// Concurrency must not race the cache fields.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ctr.diagnosticRedactor()
		}()
	}
	wg.Wait()

	ctr.clearDiagnosticSecrets()
	afterClear := ctr.diagnosticRedactor()
	if afterClear == first {
		t.Fatal("cache survived clearDiagnosticSecrets")
	}
	if got := afterClear.Text("plain-base-value"); got != "plain-base-value" {
		t.Fatalf("cleared secret was still redacted by a stale cache: %q", got)
	}
}
