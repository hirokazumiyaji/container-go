package inspect

import (
	"os"
	"testing"
)

func BenchmarkDecodeInspectOutput(b *testing.B) {
	data, err := os.ReadFile("testdata/inspect_v1.3.0.json")
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Decode(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPlatformUnmarshalJSON(b *testing.B) {
	data := []byte(`{"os":"linux","architecture":"arm64","variant":"v8"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var p Platform
		if err := p.UnmarshalJSON(data); err != nil {
			b.Fatal(err)
		}
	}
}
