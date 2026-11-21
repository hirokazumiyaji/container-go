package container

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkCopySnapshotBytes reports the copy path under its sub-benchmark
// name. On darwin, windows, and other platforms without copy_file_range
// support, copySnapshotBytes takes the pooled-buffer path; only Linux and
// FreeBSD exercise the zero-copy ReadFrom path.
func BenchmarkCopySnapshotBytes(b *testing.B) {
	path := "pooled-buffer"
	if copyReadFromSupported {
		path = "readfrom-zero-copy"
	}
	b.Run(path, func(b *testing.B) {
		const size = 4 << 20
		data := bytes.Repeat([]byte("0123456789abcdef"), size/16)
		dir := b.TempDir()
		src, err := os.OpenFile(filepath.Join(dir, "src"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			b.Fatal(err)
		}
		defer src.Close()
		if _, err := src.Write(data); err != nil {
			b.Fatal(err)
		}
		dst, err := os.OpenFile(filepath.Join(dir, "dst"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			b.Fatal(err)
		}
		defer dst.Close()

		limit := int64(len(data))
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := src.Seek(0, io.SeekStart); err != nil {
				b.Fatal(err)
			}
			if err := dst.Truncate(0); err != nil {
				b.Fatal(err)
			}
			if _, err := dst.Seek(0, io.SeekStart); err != nil {
				b.Fatal(err)
			}
			n, err := copySnapshotBytes(context.Background(), dst, src, limit)
			if err != nil {
				b.Fatal(err)
			}
			if n != int64(len(data)) {
				b.Fatalf("copied %d bytes, want %d", n, len(data))
			}
		}
	})
}
