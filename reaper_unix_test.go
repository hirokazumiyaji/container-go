//go:build darwin || linux

package container

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func reaperTestShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func reaperTestCreateFIFO(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func reaperTestOpenFIFO(t *testing.T, path string, flag int) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, flag|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func waitForReaperTestFIFOEOF(t *testing.T, file *os.File) {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		var buf [1]byte
		_, err := file.Read(buf[:])
		result <- err
	}()
	select {
	case err := <-result:
		if err != io.EOF {
			t.Fatalf("timeout helper lifetime FIFO read = %v, want EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout helper lifetime FIFO remained open")
	}
}

func waitForReaperTestReadyFiles(t *testing.T, dir string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(dir)
		count := 0
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "ready-") {
				count++
			}
		}
		if count >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	entries, _ := os.ReadDir(dir)
	t.Fatalf("timeout helper readiness files = %v, want at least %d", entries, want)
}

func TestReaperFastCallsReapTimeoutHelpers(t *testing.T) {
	dir := t.TempDir()
	callsPath := filepath.Join(dir, "calls.log")
	readyDir := filepath.Join(dir, "sleep-ready")
	currentTokenPath := filepath.Join(dir, "sleep-token")
	holdPath := filepath.Join(dir, "sleep-hold")
	exitPath := filepath.Join(dir, "sleep-exit")
	binPath := filepath.Join(dir, "container")
	sleepPath := filepath.Join(dir, "sleep")
	creation := "0123456789abcdef"
	if err := os.Mkdir(readyDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Each fake sleeper owns a writer on exitPath and blocks on holdPath.
	// The test owns both descriptors, so EOF on exitPath proves that every
	// sleeper process closed its owned handle. Cleanup closes the handles
	// directly instead of signaling a possibly recycled bare PID.
	reaperTestCreateFIFO(t, holdPath)
	holdReader := reaperTestOpenFIFO(t, holdPath, os.O_RDONLY)
	holdWriter := reaperTestOpenFIFO(t, holdPath, os.O_WRONLY)
	reaperTestCreateFIFO(t, exitPath)
	exitReader := reaperTestOpenFIFO(t, exitPath, os.O_RDONLY)

	backendScript := "#!/bin/sh\n" +
		"token=\"backend-$$\"\n" +
		"printf '%s\\n' \"$token\" > \"$REAPER_TEST_CURRENT_TOKEN.tmp\"\n" +
		"mv \"$REAPER_TEST_CURRENT_TOKEN.tmp\" \"$REAPER_TEST_CURRENT_TOKEN\"\n" +
		"i=0\n" +
		"while [ ! -e \"$REAPER_TEST_READY_DIR/ready-$token\" ]; do\n" +
		"  /bin/sleep 0.001\n" +
		"  i=$((i + 1))\n" +
		"  [ \"$i\" -lt 5000 ] || exit 1\n" +
		"done\n" +
		"/bin/sleep 0.05\n" +
		"printf '%s\\n' \"$*\" >> " + reaperTestShellQuote(callsPath) + "\n" +
		"if [ \"$1\" = inspect ]; then\n" +
		"  printf '  \"" + creationLabel + "\": \"" + creation + "\"\\n'\n" +
		"fi\n"
	if err := os.WriteFile(binPath, []byte(backendScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// The backend waits for its own sleeper's readiness file before
	// returning. The token handoff also handles the scheduler case where
	// the timeout helper starts before the backend gets CPU.
	sleepScript := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ ! -s \"$REAPER_TEST_CURRENT_TOKEN\" ]; do\n" +
		"  /bin/sleep 0.001\n" +
		"  i=$((i + 1))\n" +
		"  [ \"$i\" -lt 5000 ] || exit 1\n" +
		"done\n" +
		"token=$(sed -n '1p' \"$REAPER_TEST_CURRENT_TOKEN\")\n" +
		"exec 3>\"$REAPER_TEST_EXIT_FIFO\"\n" +
		"printf '%s\\n' \"$$\" > \"$REAPER_TEST_READY_DIR/ready-$token\"\n" +
		"IFS= read -r _ < \"$REAPER_TEST_HOLD_FIFO\"\n"
	if err := os.WriteFile(sleepPath, []byte(sleepScript), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newReaper(binPath, "delete")
	r.command = func() *exec.Cmd {
		cmd := exec.Command("/bin/sh", "-c", reaperScript, "containergo-reaper", binPath, "delete", breQuote(creationLabel))
		cmd.Env = append(os.Environ(),
			"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"REAPER_TEST_CURRENT_TOKEN="+currentTokenPath,
			"REAPER_TEST_READY_DIR="+readyDir,
			"REAPER_TEST_HOLD_FIFO="+holdPath,
			"REAPER_TEST_EXIT_FIFO="+exitPath,
		)
		return cmd
	}
	t.Cleanup(func() {
		r.closeStdin()
		waitForReaperExit(t, r)
	})
	t.Cleanup(func() {
		_ = holdWriter.Close()
		_ = holdReader.Close()
		_ = exitReader.Close()
	})

	if err := r.register("guarded", creation); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.closeStdin()
	waitForLogLines(t, callsPath, "inspect guarded", "delete --force guarded")
	waitForReaperExit(t, r)
	waitForReaperTestReadyFiles(t, readyDir, 2)
	waitForReaperTestFIFOEOF(t, exitReader)
}
