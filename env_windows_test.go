//go:build windows

package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsEnvFileUsesProtectedCurrentUserDACL(t *testing.T) {
	isolateEnvFileRoot(t)
	path, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret=value"})
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanupEnvFile(dir); err != nil {
			t.Errorf("cleanupEnvFile: %v", err)
		}
	})

	if err := verifyWindowsPrivateDir(dir); err != nil {
		t.Fatalf("env directory DACL: %v", err)
	}
	if err := verifyWindowsPrivateFile(path); err != nil {
		t.Fatalf("env file DACL: %v", err)
	}
	if err := verifyWindowsEnvDirContents(dir, true); err != nil {
		t.Fatalf("env directory contents: %v", err)
	}
	data, err := readWindowsPrivateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "TOKEN=secret=value\n"; got != want {
		t.Errorf("env file = %q, want %q", got, want)
	}
}

func TestWindowsEnvFileCleanupRemovesAllArtifacts(t *testing.T) {
	isolateEnvFileRoot(t)
	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanupEnvFile(dir); err != nil {
		t.Fatalf("cleanupEnvFile: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("environment directory remains after cleanup: %v", err)
	}
}

func TestWindowsEnvFileStaleCleanupSkipsLiveWriter(t *testing.T) {
	isolateEnvFileRoot(t)
	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanupEnvFile(dir); err != nil {
			t.Errorf("cleanupEnvFile: %v", err)
		}
	})

	if err := cleanupStaleEnvFilesContext(context.Background()); err != nil {
		t.Fatalf("cleanupStaleEnvFilesContext: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("live env directory was removed: %v", err)
	}
}

func TestWindowsEnvFileStaleCleanupReclaimsCrashedWriter(t *testing.T) {
	isolateEnvFileRoot(t)
	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	simulateCrashedWindowsEnvWriter(t, dir)

	if err := cleanupStaleEnvFilesContext(context.Background()); err != nil {
		t.Fatalf("cleanupStaleEnvFilesContext: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("crashed env directory was not reclaimed: %v", err)
	}
}

// TestWindowsEnvFileCleanupHoldsWriterLockDuringRemoval pins the ordering that
// keeps a stale scan from observing a half-removed directory: the owner
// deletes the env file and marker while still holding the writer lock, and the
// scan skips the entry because it cannot take that lock.
func TestWindowsEnvFileCleanupHoldsWriterLockDuringRemoval(t *testing.T) {
	isolateEnvFileRoot(t)
	path, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		windowsEnvCleanupBeforeLockCloseHook = nil
		_ = cleanupEnvFile(dir)
	})

	var scanErr error
	var sawDir bool
	windowsEnvCleanupBeforeLockCloseHook = func(hookDir string) {
		windowsEnvCleanupBeforeLockCloseHook = nil
		if hookDir != dir {
			t.Errorf("hook dir = %q, want %q", hookDir, dir)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("env file still present inside hook: %v", err)
		}
		_, lockErr := os.Stat(filepath.Join(dir, envFileLockName))
		sawDir = lockErr == nil
		// Hide the registry entry so the scan must rely on the held writer
		// lock, which is the mechanism a concurrent process would encounter.
		windowsEnvMu.Lock()
		saved := windowsEnvFiles[dir]
		delete(windowsEnvFiles, dir)
		windowsEnvMu.Unlock()
		scanErr = cleanupStaleEnvFilesContext(context.Background())
		windowsEnvMu.Lock()
		if saved != nil {
			windowsEnvFiles[dir] = saved
		}
		windowsEnvMu.Unlock()
	}
	if err := cleanupEnvFile(dir); err != nil {
		t.Fatalf("cleanupEnvFile: %v", err)
	}
	if !sawDir {
		t.Error("writer lock file was already gone before the lock handle closed")
	}
	if scanErr != nil {
		t.Fatalf("stale scan raced the owner cleanup: %v", scanErr)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("environment directory remains: %v", err)
	}
}

// TestWindowsEnvFileConcurrentWriteCleanupAndScan runs writers and stale scans
// against the same root. A scan must never fail on a directory that another
// goroutine is finishing, which is the race M1 reported.
func TestWindowsEnvFileConcurrentWriteCleanupAndScan(t *testing.T) {
	isolateEnvFileRoot(t)
	const (
		workers    = 4
		iterations = 8
	)
	var (
		writers sync.WaitGroup
		scanner sync.WaitGroup
	)
	stop := make(chan struct{})
	errs := make(chan error, workers*iterations+1)

	scanner.Add(1)
	go func() {
		defer scanner.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := cleanupStaleEnvFilesContext(context.Background()); err != nil {
				errs <- err
				return
			}
		}
	}()
	for i := 0; i < workers; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < iterations; j++ {
				path, dir, err := writeEnvFileContext(context.Background(), map[string]string{"TOKEN": "secret"})
				if err != nil {
					errs <- err
					return
				}
				if _, err := os.ReadFile(path); err != nil {
					errs <- err
					return
				}
				if err := cleanupEnvFile(dir); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	writers.Wait()
	close(stop)
	scanner.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent env file operation failed: %v", err)
	}
}

func TestWindowsEnvFileCleanupRejectsUnexpectedChild(t *testing.T) {
	isolateEnvFileRoot(t)
	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(dir, "unexpected"))
		if err := cleanupEnvFile(dir); err != nil {
			t.Errorf("cleanupEnvFile: %v", err)
		}
	})

	if err := os.WriteFile(filepath.Join(dir, "unexpected"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = cleanupEnvFile(dir)
	if !errors.Is(err, errUnsafeEnvFile) {
		t.Fatalf("cleanup error = %v, want errUnsafeEnvFile", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("unexpected child caused unconditional removal: %v", err)
	}
}

func TestWindowsEnvFileRejectsReparseRoot(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	root := filepath.Join(base, envFileRootName)
	if err := os.Symlink(target, root); err != nil {
		t.Skipf("Windows symlink unavailable: %v", err)
	}

	_, _, err := writeEnvFileAtContext(context.Background(), base, map[string]string{"TOKEN": "secret"})
	if !errors.Is(err, errUnsafeEnvFile) {
		t.Fatalf("writeEnvFileAtContext error = %v, want errUnsafeEnvFile", err)
	}
}

func TestWindowsEnvFileRejectsForeignOwner(t *testing.T) {
	isolateEnvFileRoot(t)
	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanupEnvFile(dir) })
	foreignSID, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Skipf("foreign SID unavailable: %v", err)
	}
	currentUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
		foreignSID,
		nil,
		nil,
		nil,
	); err != nil {
		t.Skipf("cannot replace directory owner: %v", err)
	}
	t.Cleanup(func() {
		_ = windows.SetNamedSecurityInfo(
			dir,
			windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION,
			currentUser.User.Sid,
			nil,
			nil,
			nil,
		)
		_ = cleanupEnvFile(dir)
	})

	if err := cleanupEnvFile(dir); !errors.Is(err, errUnsafeEnvFile) {
		t.Fatalf("cleanup error = %v, want errUnsafeEnvFile", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("foreign-owned directory was removed: %v", err)
	}
}

// TestWindowsEnvFileAcceptsAdministratorsOwner pins the copy-staging owner
// policy: GitHub-hosted runners can leave new objects owned by the
// Administrators group, which the DACL allowlist already trusts.
func TestWindowsEnvFileAcceptsAdministratorsOwner(t *testing.T) {
	isolateEnvFileRoot(t)
	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanupEnvFile(dir) })
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Skipf("administrators SID unavailable: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
		adminSID,
		nil,
		nil,
		nil,
	); err != nil {
		t.Skipf("cannot replace directory owner: %v", err)
	}
	if err := verifyWindowsPrivateDir(dir); err != nil {
		t.Fatalf("Administrators-owned private directory was rejected: %v", err)
	}
}

// TestWindowsEnvFileStaleCleanupReclaimsPartialMarker covers a crash during the
// marker write, before the writer lock exists. A prefix of the marker holds
// no secret and must not make every later Run fail closed.
func TestWindowsEnvFileStaleCleanupReclaimsPartialMarker(t *testing.T) {
	isolateEnvFileRoot(t)
	path, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	simulateCrashedWindowsEnvWriter(t, dir)
	if err := os.Remove(filepath.Join(dir, envFileLockName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, envFileDirMarkerName), []byte("container"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	if err := cleanupStaleEnvFilesContext(context.Background()); err != nil {
		t.Fatalf("cleanupStaleEnvFilesContext: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("partially marked env directory was not reclaimed: %v", err)
	}
}

func TestWindowsEnvFileCleanupRetriesTransientSharingViolation(t *testing.T) {
	isolateEnvFileRoot(t)
	path, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	// Hold the env file without FILE_SHARE_DELETE, like an antivirus scanner
	// would, so the first removal attempts fail with a sharing violation.
	release := holdWindowsEnvFile(t, path)
	go func() {
		time.Sleep(250 * time.Millisecond)
		release()
	}()

	// While the owner backs off it keeps the writer lock, so a stale scan must
	// skip the directory instead of failing on the torn state.
	scanErr := make(chan error, 1)
	stopScan := make(chan struct{})
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		for {
			select {
			case <-stopScan:
				return
			default:
			}
			if err := cleanupStaleEnvFilesContext(context.Background()); err != nil {
				scanErr <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	cleanupErr := cleanupEnvFile(dir)
	close(stopScan)
	<-scanDone
	select {
	case err := <-scanErr:
		t.Fatalf("stale scan failed during cleanup backoff: %v", err)
	default:
	}
	if cleanupErr != nil {
		t.Fatalf("cleanupEnvFile: %v", cleanupErr)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("environment directory remains after transient retry: %v", err)
	}
}

// TestWindowsEnvFileCleanupBudgetExhaustionReleasesLock pins both halves of
// the invariant: a failed env-file removal never touches the marker, and a
// budget-exhausted cleanup releases the writer lock so the next scan (in this
// process too) can reclaim the directory.
func TestWindowsEnvFileCleanupBudgetExhaustionReleasesLock(t *testing.T) {
	isolateEnvFileRoot(t)
	oldTimeout := windowsEnvCleanupTimeout
	windowsEnvCleanupTimeout = 60 * time.Millisecond
	t.Cleanup(func() { windowsEnvCleanupTimeout = oldTimeout })

	path, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanupEnvFile(dir) })
	release := holdWindowsEnvFile(t, path)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		time.Sleep(800 * time.Millisecond)
		release()
	}()

	if err := cleanupEnvFile(dir); err == nil {
		t.Fatal("cleanup succeeded while the env file was still locked")
	}
	if _, err := os.Stat(filepath.Join(dir, envFileDirMarkerName)); err != nil {
		t.Fatalf("marker was removed after a failed cleanup: %v", err)
	}
	if state := loadWindowsEnvState(dir); state != nil {
		t.Fatal("writer lock state remained registered after the retry budget was exhausted")
	}
	lock, err := openWindowsPrivateFileHandle(filepath.Join(dir, envFileLockName), windows.GENERIC_READ|windows.GENERIC_WRITE)
	if err != nil {
		t.Fatalf("writer lock was not released: %v", err)
	}
	locked, err := tryAcquireWindowsFileLock(lock)
	_ = lock.Close()
	if err != nil || !locked {
		t.Fatalf("writer lock was not released: locked=%v err=%v", locked, err)
	}
	// The env file is still held, so a scan skips it without failing.
	if err := cleanupStaleEnvFilesContext(context.Background()); err != nil {
		t.Fatalf("stale scan failed while the env file was still held: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, envFileDirMarkerName)); err != nil {
		t.Fatalf("marker was removed by the skipped scan: %v", err)
	}
	<-closed
	// With the lock released and the sharing violation gone, the next scan in
	// the same process reclaims the normal-shaped directory.
	if err := cleanupStaleEnvFilesContext(context.Background()); err != nil {
		t.Fatalf("cleanupStaleEnvFilesContext after recovery: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("released environment directory was not reclaimed by the next scan: %v", err)
	}
}

// TestWindowsEnvFileStaleCleanupSkipsTransientRemovalFailure checks the scan
// side of the same invariant: a transient failure stops the sequential
// removal, the marker stays, and a later scan reclaims the directory.
func TestWindowsEnvFileStaleCleanupSkipsTransientRemovalFailure(t *testing.T) {
	isolateEnvFileRoot(t)
	path, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	simulateCrashedWindowsEnvWriter(t, dir)
	release := holdWindowsEnvFile(t, path)

	if err := cleanupStaleEnvFilesContext(context.Background()); err != nil {
		t.Fatalf("cleanupStaleEnvFilesContext: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, envFileDirMarkerName)); err != nil {
		t.Fatalf("marker was removed by a failed scan: %v", err)
	}
	release()
	if err := cleanupStaleEnvFilesContext(context.Background()); err != nil {
		t.Fatalf("cleanupStaleEnvFilesContext after release: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("crashed env directory was not reclaimed: %v", err)
	}
}

// TestWindowsEnvFileCleanupWaitsForScanOwnedRemoval pins the n1 behaviour: the
// owner cleanup does not report success while a stale scan holds the writer
// lock; it retries and only succeeds once the directory is gone.
func TestWindowsEnvFileCleanupWaitsForScanOwnedRemoval(t *testing.T) {
	isolateEnvFileRoot(t)
	_, dir, err := writeEnvFile(map[string]string{"TOKEN": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	simulateCrashedWindowsEnvWriter(t, dir)
	holder, err := openWindowsPrivateFileHandle(filepath.Join(dir, envFileLockName), windows.GENERIC_READ|windows.GENERIC_WRITE)
	if err != nil {
		t.Fatal(err)
	}
	if locked, err := tryAcquireWindowsFileLock(holder); err != nil || !locked {
		_ = holder.Close()
		t.Fatalf("could not hold the writer lock: locked=%v err=%v", locked, err)
	}
	done := make(chan error, 1)
	go func() { done <- cleanupEnvFile(dir) }()
	select {
	case err := <-done:
		t.Fatalf("cleanup returned %v while a scan held the writer lock", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cleanupEnvFile: %v", err)
		}
	case <-time.After(windowsEnvCleanupTimeout + 2*time.Second):
		t.Fatal("cleanup did not finish after the scan released the lock")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("environment directory remains after scan-owned removal: %v", err)
	}
}

// TestWindowsPrivateOwnerAllowlist pins the owner policy without relying on
// privileges needed to change a real object's owner.
func TestWindowsPrivateOwnerAllowlist(t *testing.T) {
	allowed, err := allowedPrivateSIDs()
	if err != nil {
		t.Fatal(err)
	}
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Skipf("Users SID unavailable: %v", err)
	}
	if sidAllowedForPrivate(users, allowed) {
		t.Error("BUILTIN\\Users is allowed as an environment object owner")
	}
	admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Skipf("Administrators SID unavailable: %v", err)
	}
	if !sidAllowedForPrivate(admin, allowed) {
		t.Error("BUILTIN\\Administrators is not allowed as an environment object owner")
	}
}

func holdWindowsEnvFile(t *testing.T, path string) func() {
	t.Helper()
	pathPtr, err := windows.UTF16PtrFromString(windowsExtendedPath(path))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = windows.CloseHandle(handle) }) }
	t.Cleanup(release)
	return release
}

func TestWindowsEnvValidationRejectsBeforeBackendCall(t *testing.T) {
	isolateEnvFileRoot(t)
	f := newTestRunner()
	_, err := Run(context.Background(), "redis:7-alpine",
		WithName("myctr"), WithEnv(map[string]string{"BAD KEY": "x"}),
		withRunner(f), withEngine(dockerEngine{}))
	if err == nil {
		t.Fatal("invalid environment entry was accepted")
	}
	if len(f.calls) != 0 {
		t.Fatalf("backend was called despite invalid environment entry: %v", f.calls)
	}
}

func TestWindowsRunPassesEnvFileContent(t *testing.T) {
	isolateEnvFileRoot(t)
	f := newTestRunner()
	runTestContainer(t, f, WithEnv(map[string]string{"PASSWORD": "s3cret"}))

	runCall := f.callWith("run")
	for _, arg := range runCall {
		if arg == "--env" {
			t.Errorf("env passed via argv: %v", runCall)
		}
	}
	if len(f.envFiles) != 1 {
		t.Fatalf("env file captures = %d, want 1", len(f.envFiles))
	}
	if !strings.Contains(f.envFiles[0], "PASSWORD=s3cret\n") {
		t.Errorf("env-file content = %q", f.envFiles[0])
	}
}

// simulateCrashedWindowsEnvWriter drops the in-process writer lock without
// cleaning up, which is the state a stale scan must reclaim.
func simulateCrashedWindowsEnvWriter(t *testing.T, dir string) {
	t.Helper()
	state := takeWindowsEnvState(dir)
	if state == nil {
		t.Fatalf("no active writer state for %q", dir)
	}
	lock := state.takeLock()
	if lock == nil {
		t.Fatalf("no writer lock for %q", dir)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
