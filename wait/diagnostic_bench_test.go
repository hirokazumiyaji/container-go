package wait

import (
	"errors"
	"testing"
)

func BenchmarkSafeDiagnosticErrorSuccess(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := safeDiagnosticError(nil, "port"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSafeDiagnosticErrorFailure(b *testing.B) {
	sticky := errors.New("probe failed")
	values := []string{"6379/tcp", "postgres", "SELECT 1"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := safeDiagnosticError(sticky, values...); err == nil {
			b.Fatal("want a wrapped error")
		}
	}
}

func BenchmarkSafeStrategyErrorSuccess(b *testing.B) {
	strategy := ForListeningPort("6379/tcp")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := safeStrategyError(nil, strategy); err != nil {
			b.Fatal(err)
		}
	}
}
