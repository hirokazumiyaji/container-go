package diagnostic

import "testing"

func BenchmarkRedactorTextNoSecret(b *testing.B) {
	r := NewHashedContextRedactor("session-token-value")
	line := "level=info msg=\"request handled\" method=GET path=/v1/items/42 status=200 duration=12ms"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := r.Text(line); got == "" {
			b.Fatal("empty text")
		}
	}
}

func BenchmarkRedactorTextPlain(b *testing.B) {
	r := NewHashedContextRedactor("session-token-value")
	line := "dial tcp 127.0.0.1:2375: connect: connection refused"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := r.Text(line); got == "" {
			b.Fatal("empty text")
		}
	}
}

func BenchmarkSanitizeClean(b *testing.B) {
	line := "level=info msg=\"request handled\" method=GET path=/v1/items/42 status=200"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := Sanitize(line); got == "" {
			b.Fatal("empty text")
		}
	}
}
