package strictjson

import (
	"fmt"
	"testing"
)

type benchEntry struct {
	ID     NonNull[string] `json:"id"`
	Status NonNull[struct {
		State NonNull[string] `json:"state"`
	}] `json:"status"`
	CapAdd    []string          `json:"capAdd"`
	Labels    map[string]string `json:"labels"`
	Published []string          `json:"publishedPorts"`
}

func benchmarkArrayData() []byte {
	// Real Docker inspect output almost always carries explicit nulls in
	// nullable collections such as CapAdd. Including them here measures the
	// path production actually takes.
	data := make([]byte, 0, 240*200)
	data = append(data, '[')
	for i := 0; i < 200; i++ {
		if i > 0 {
			data = append(data, ',')
		}
		data = fmt.Appendf(data, `{"id":"container-%d","capAdd":null,"capDrop":null,"labels":{"role":"test"},"status":{"state":"running"},"publishedPorts":null}`, i)
	}
	return append(data, ']')
}

var benchRequired = []string{"id", "status", "status.state"}

func BenchmarkArray(b *testing.B) {
	data := benchmarkArrayData()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries, err := Array[benchEntry](data, benchRequired)
		if err != nil {
			b.Fatal(err)
		}
		if len(entries) != 200 {
			b.Fatalf("len = %d, want 200", len(entries))
		}
	}
}

func BenchmarkArrayNoNulls(b *testing.B) {
	// The same shape without nulls documents the fast path the old decoder
	// already took; the gap between the two is the extra pass this change
	// removes for real output.
	data := make([]byte, 0, 240*200)
	data = append(data, '[')
	for i := 0; i < 200; i++ {
		if i > 0 {
			data = append(data, ',')
		}
		data = fmt.Appendf(data, `{"id":"container-%d","capAdd":[],"capDrop":[],"labels":{"role":"test"},"status":{"state":"running"},"publishedPorts":[]}`, i)
	}
	data = append(data, ']')
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries, err := Array[benchEntry](data, benchRequired)
		if err != nil {
			b.Fatal(err)
		}
		if len(entries) != 200 {
			b.Fatalf("len = %d, want 200", len(entries))
		}
	}
}
