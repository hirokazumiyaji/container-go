//go:build !windows

package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkEnvFileWriteDue(b *testing.B) {
	benchmarkEnvFileWrite(b, true)
}

func BenchmarkEnvFileWriteThrottled(b *testing.B) {
	benchmarkEnvFileWrite(b, false)
}

// benchmarkEnvFileWrite measures one env write plus its cleanup. "Due" resets
// the per-root reclaim hint before every iteration, which is the unconditional
// scan the write path performed before the hint existed. "Throttled" marks a
// successful scan as just completed, which is the steady state of the
// throttled write path.
func benchmarkEnvFileWrite(b *testing.B, due bool) {
	base := b.TempDir()
	root := benchmarkEnvRoot(b, base)
	env := map[string]string{"TOKEN": "secret"}
	b.ReportAllocs()
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		setBenchmarkEnvDirGCDue(root, due)
		b.StartTimer()
		_, dir, err := writeEnvFileAt(base, env)
		if err != nil {
			b.Fatal(err)
		}
		if err := cleanupEnvFileAt(context.Background(), dir, envFileCleanupOptions{base: base}); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
	}
}

func BenchmarkEnvFileWithStaleDirsDue(b *testing.B) {
	benchmarkEnvFileWithStaleDirs(b, true)
}

func BenchmarkEnvFileWithStaleDirsThrottled(b *testing.B) {
	benchmarkEnvFileWithStaleDirs(b, false)
}

// benchmarkEnvFileWithStaleDirs measures one env write while 32 abandoned
// directories wait to be reclaimed. In the due case the timed write reclaims
// them; in the throttled case the write skips the scan and setup removes them
// outside the timer. Setup and teardown stay outside the timer in both cases
// so the result isolates the reclaim scan.
func benchmarkEnvFileWithStaleDirs(b *testing.B, due bool) {
	base := b.TempDir()
	root := benchmarkEnvRoot(b, base)
	env := map[string]string{"TOKEN": "secret"}
	b.ReportAllocs()
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		setBenchmarkEnvDirGCDue(root, due)
		stale := make([]string, 32)
		for j := range stale {
			stale[j] = newBenchmarkStaleEnvDir(b, root, fmt.Sprintf("bench-%d-%d", i, j))
		}
		b.StartTimer()
		_, dir, err := writeEnvFileAt(base, env)
		if err != nil {
			b.Fatal(err)
		}
		if err := cleanupEnvFileAt(context.Background(), dir, envFileCleanupOptions{base: base}); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if due {
			for _, dir := range stale {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					b.Fatalf("reclaim scan left stale directory %s: %v", dir, err)
				}
			}
			continue
		}
		for _, dir := range stale {
			_ = os.RemoveAll(dir)
		}
	}
}

// benchmarkEnvRoot creates the private root once and returns the canonical
// spelling the write path uses as the reclaim-hint key.
func benchmarkEnvRoot(b *testing.B, base string) string {
	b.Helper()
	_, dir, err := writeEnvFileAt(base, map[string]string{"BOOT": "1"})
	if err != nil {
		b.Fatal(err)
	}
	if err := cleanupEnvFileAt(context.Background(), dir, envFileCleanupOptions{base: base}); err != nil {
		b.Fatal(err)
	}
	root, err := canonicalEnvPath(filepath.Join(base, envFileRootName))
	if err != nil {
		b.Fatal(err)
	}
	return root
}

func setBenchmarkEnvDirGCDue(root string, due bool) {
	envDirGCMu.Lock()
	defer envDirGCMu.Unlock()
	if due {
		delete(envDirGCLast, root)
		return
	}
	envDirGCLast[root] = time.Now()
}

func newBenchmarkStaleEnvDir(b *testing.B, root, suffix string) string {
	b.Helper()
	dir := filepath.Join(root, envFileDirPrefix+suffix)
	if err := os.Mkdir(dir, envDirMode); err != nil {
		b.Fatal(err)
	}
	if err := writeEnvMarker(filepath.Join(dir, envFileDirMarkerName)); err != nil {
		b.Fatal(err)
	}
	envFile, err := os.OpenFile(filepath.Join(dir, envFileName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, envFileMode)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := envFile.WriteString("TOKEN=secret\n"); err != nil {
		b.Fatal(err)
	}
	if err := envFile.Close(); err != nil {
		b.Fatal(err)
	}
	lock, err := openEnvFileNoFollow(filepath.Join(dir, envFileLockName), os.O_CREATE|os.O_RDWR, envFileMode)
	if err != nil {
		b.Fatal(err)
	}
	if err := closeEnvFileLock(lock); err != nil {
		b.Fatal(err)
	}
	return dir
}
