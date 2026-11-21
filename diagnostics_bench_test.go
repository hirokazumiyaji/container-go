package container

import (
	"net/netip"
	"strings"
	"testing"
)

func BenchmarkContainerDiagnosticRedactor(b *testing.B) {
	ctr := &Container{
		id:        strings.Repeat("a", 64),
		exposed:   []portSpec{{Port: 6379, Proto: "tcp"}, {Port: 8080, Proto: "tcp"}},
		published: []publishSpec{{HostAddr: netip.MustParseAddr("127.0.0.1"), HostPort: 16379, ContainerPort: 6379, Proto: "tcp", Raw: "127.0.0.1:16379:6379/tcp"}},
		diagnosticRedactorValue: newDiagnosticMatcher([]string{
			"session-token-value", "container-secret-value", "127.0.0.1:16379:6379/tcp",
		}),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ctr.diagnosticRedactor() == nil {
			b.Fatal("nil redactor")
		}
	}
}
