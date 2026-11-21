package strictjson

import (
	"fmt"
	"testing"
)

type benchEntry struct {
	ID     string `json:"id"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

func benchmarkArrayData() []byte {
	data := make([]byte, 0, 200*120)
	data = append(data, '[')
	for i := 0; i < 200; i++ {
		if i > 0 {
			data = append(data, ',')
		}
		data = fmt.Appendf(data, `{"id":"container-%d","status":{"state":"running"}}`, i)
	}
	return append(data, ']')
}

func BenchmarkArray(b *testing.B) {
	data := benchmarkArrayData()
	required := []string{"id", "status.state"}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries, err := Array[benchEntry](data, required)
		if err != nil {
			b.Fatal(err)
		}
		if len(entries) != 200 {
			b.Fatalf("len = %d, want 200", len(entries))
		}
	}
}
