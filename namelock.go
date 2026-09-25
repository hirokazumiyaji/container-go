//go:build !windows

package container

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	nameLockDirPerm         = 0o700
	nameLockFilePerm        = 0o600
	nameLockPoll            = 10 * time.Millisecond
	nameLockRetention       = 7 * 24 * time.Hour
	nameLockMaxFiles        = 256
	nameLockMaxLeases       = 256
	nameLockCleanupScan     = nameLockMaxFiles + 2
	nameLockCleanupDelete   = 32
	nameLockMaintenanceFile = ".maintenance.lock"
	nameLockLeaseSuffix     = ".reaper-lease"
	nameLockLeaseHoldSuffix = nameLockLeaseSuffix + "."
	nameLockActiveSuffix    = ".reaper-active"
)

type nameLockStage string

const (
	legacyNameLockStage      nameLockStage = "legacy TMPDIR"
	cacheNameLockStage       nameLockStage = "transitional UserCacheDir"
	stateNameLockStage       nameLockStage = "durable state"
	maintenanceNameLockStage nameLockStage = "lock maintenance"
)

// nameLockHooks exists so tests can place a substitution at each side of
// the final-component open without mutating process-global test state.
type nameLockHooks struct {
	beforeOpen    func(nameLockStage, string)
	afterOpen     func(nameLockStage, string)
	beforeCleanup func()
}

// nameLockStateRootOverride is used only by the package test harness to
// keep lock files out of the developer's real account state directory.
// Production callers leave it empty and always derive the state root from
// the account database.
var nameLockStateRootOverride string

// nameLockPath returns the durable, per-user state path for name. If a
// reaper lease exists, its hard link is the canonical path so a normal
// caller and the reaper continue to use the same inode.
func nameLockPath(name string) (string, error) {
	raw, err := rawNameLockPath(name)
	if err != nil {
		return "", err
	}
	return canonicalNameLockPath(raw)
}

func rawNameLockPath(name string) (string, error) {
	dir, err := nameLockDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, nameLockFileName(name)), nil
}

// canonicalNameLockPath returns a durable reaper lease when one is
// present. A lease is a hard link to the original lock inode, so cleanup
// may retain the lease while the replaceable original path is recreated.
// The fixed lease name is retained for the shell protocol; per-entry
// hold links make ownership and reclamation safe when several reapers
// use the same name.
func canonicalNameLockPath(path string) (string, error) {
	leases, err := reaperLeasePaths(path)
	if err != nil {
		return "", err
	}
	if len(leases) != 0 {
		return leases[0], nil
	}
	return path, nil
}

func validReaperLeaseToken(token string) bool {
	if len(token) != 32 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func reaperLeasePaths(rawPath string) ([]string, error) {
	dir := filepath.Dir(rawPath)
	base := filepath.Base(rawPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fixedName := base + nameLockLeaseSuffix
	holdPrefix := base + nameLockLeaseHoldSuffix
	var paths []string
	originalInfo, originalErr := os.Lstat(rawPath)
	if originalErr == nil {
		if err := checkLockFile(originalInfo, rawPath); err != nil {
			return nil, err
		}
	} else if !errors.Is(originalErr, fs.ErrNotExist) {
		return nil, originalErr
	}
	for _, entry := range entries {
		name := entry.Name()
		var candidate string
		switch {
		case name == fixedName:
			candidate = filepath.Join(dir, name)
		case strings.HasPrefix(name, holdPrefix):
			token := strings.TrimPrefix(name, holdPrefix)
			if !validReaperLeaseToken(token) {
				return nil, fmt.Errorf("invalid reaper lease hold name %s", name)
			}
			candidate = filepath.Join(dir, name)
		default:
			continue
		}
		leaseInfo, statErr := os.Lstat(candidate)
		if errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, fmt.Errorf("stat reaper lease %s: %w", candidate, statErr)
		}
		if err := checkLockFile(leaseInfo, candidate); err != nil {
			return nil, fmt.Errorf("validate reaper lease %s: %w", candidate, err)
		}
		if originalInfo != nil && !os.SameFile(originalInfo, leaseInfo) {
			return nil, fmt.Errorf("reaper lease %s does not match lock inode %s", candidate, rawPath)
		}
		paths = append(paths, candidate)
	}
	sort.Strings(paths)
	return paths, nil
}

// reaperNameLockSet preserves the package helper shape. It prepares the
// shared fixed lease but does not claim an ownership hold; the reaper
// registration path below adds one hold per entry so reclamation can be
// reference-counted across processes.
func reaperNameLockSet(name string) ([]string, []string, error) {
	paths, identities, _, err := reaperNameLockSetWithOwnership(name, false)
	return paths, identities, err
}

func reaperNameLockSetForReaper(name string) ([]string, []string, []string, error) {
	return reaperNameLockSetWithOwnership(name, true)
}

// reaperActiveMarkerPath is a process-level liveness hold in the durable
// lock directory. Its contents list the state-lock names currently covered
// by the reaper; it is separate from the hard-link leases so holding it
// never blocks ordinary name-lock callers.
func reaperActiveMarkerPath(dir, token string) string {
	return filepath.Join(dir, nameLockActiveSuffix+"."+token)
}

type reaperActiveHold struct {
	path     string
	identity string
	file     *os.File
	raws     map[string]int
}

func newReaperActiveHoldLocked(dir, token string) (*reaperActiveHold, error) {
	if !validReaperLeaseToken(token) {
		return nil, fmt.Errorf("invalid reaper active marker token")
	}
	path := reaperActiveMarkerPath(dir, token)
	f, err := openLockFile(path, true, stateNameLockStage, nil)
	if err != nil {
		return nil, fmt.Errorf("create reaper active marker: %w", err)
	}
	closeAndUnlock := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	if err := checkOpenedLockFile(f, path); err != nil {
		closeAndUnlock()
		return nil, fmt.Errorf("validate reaper active marker: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		closeAndUnlock()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("reaper active marker %s is busy", path)
		}
		return nil, fmt.Errorf("lock reaper active marker: %w", err)
	}
	if err := checkOpenedLockFile(f, path); err != nil {
		closeAndUnlock()
		return nil, fmt.Errorf("revalidate reaper active marker: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		closeAndUnlock()
		return nil, err
	}
	identity, err := lockFileIdentity(info)
	if err != nil {
		closeAndUnlock()
		return nil, err
	}
	return &reaperActiveHold{path: path, identity: identity, file: f, raws: make(map[string]int)}, nil
}

func acquireActiveMaintenance(dir string) (func(), error) {
	var last error
	for attempt := 0; attempt < nameLockLeaseRetryLimit; attempt++ {
		maintenancePath, err := maintenanceNameLockPath(dir)
		if err != nil {
			last = err
			if !errors.Is(err, errNameLockPathChanged) {
				return nil, err
			}
			time.Sleep(nameLockPoll)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
		unlock, err := acquireLockFile(ctx, maintenanceNameLockStage, maintenancePath, false, nil)
		cancel()
		if err == nil {
			return unlock, nil
		}
		last = err
		if !errors.Is(err, errNameLockPathChanged) {
			return nil, err
		}
		time.Sleep(nameLockPoll)
	}
	return nil, last
}

func newReaperActiveHold(dir string) (*reaperActiveHold, error) {
	token, err := newReaperLeaseToken()
	if err != nil {
		return nil, err
	}
	maintenanceUnlock, err := acquireActiveMaintenance(dir)
	if err != nil {
		return nil, err
	}
	hold, err := newReaperActiveHoldLocked(dir, token)
	maintenanceUnlock()
	return hold, err
}

func (h *reaperActiveHold) writeRawsLocked() error {
	if err := checkOpenedLockFile(h.file, h.path); err != nil {
		return err
	}
	names := make([]string, 0, len(h.raws))
	for raw := range h.raws {
		names = append(names, raw)
	}
	sort.Strings(names)
	body := strings.Join(names, "\n")
	checksum := sha256.Sum256([]byte(body))
	contents := fmt.Sprintf("v1 %d %s\n%s", len(names), hex.EncodeToString(checksum[:]), body)
	if err := h.file.Truncate(0); err != nil {
		return fmt.Errorf("truncate reaper active marker: %w", err)
	}
	if _, err := h.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind reaper active marker: %w", err)
	}
	if _, err := io.WriteString(h.file, contents); err != nil {
		return fmt.Errorf("write reaper active marker: %w", err)
	}
	if err := h.file.Sync(); err != nil {
		return fmt.Errorf("sync reaper active marker: %w", err)
	}
	return checkOpenedLockFile(h.file, h.path)
}

func (h *reaperActiveHold) updateRaw(raw string, add, barriersHeld bool) error {
	if h == nil || h.file == nil {
		return fmt.Errorf("reaper active marker is not open")
	}
	raw = filepath.Base(raw)
	if !isNameLockFile(raw) {
		return fmt.Errorf("invalid reaper active raw lock %q", raw)
	}
	if !barriersHeld {
		maintenanceUnlock, err := acquireActiveMaintenance(filepath.Dir(h.path))
		if err != nil {
			return err
		}
		defer maintenanceUnlock()
	}
	if h.raws == nil {
		h.raws = make(map[string]int)
	}
	oldCount := h.raws[raw]
	newCount := oldCount
	if add {
		newCount++
	} else if oldCount > 1 {
		newCount--
	} else {
		newCount = 0
	}
	if newCount == 0 {
		delete(h.raws, raw)
	} else {
		h.raws[raw] = newCount
	}
	if err := h.writeRawsLocked(); err != nil {
		if oldCount == 0 {
			delete(h.raws, raw)
		} else {
			h.raws[raw] = oldCount
		}
		return err
	}
	return nil
}

func (h *reaperActiveHold) addRaw(raw string) error {
	return h.updateRaw(raw, true, false)
}

func (h *reaperActiveHold) removeRaw(raw string, barriersHeld bool) error {
	return h.updateRaw(raw, false, barriersHeld)
}

func (h *reaperActiveHold) release(barriersHeld bool) error {
	if h == nil || h.file == nil {
		return nil
	}
	if !barriersHeld {
		maintenanceUnlock, err := acquireActiveMaintenance(filepath.Dir(h.path))
		if err != nil {
			_ = syscall.Flock(int(h.file.Fd()), syscall.LOCK_UN)
			closeErr := h.file.Close()
			h.file = nil
			return errors.Join(err, closeErr)
		}
		defer maintenanceUnlock()
	}
	removeErr := removeReaperLeaseFile(h.path, h.identity, true)
	_ = syscall.Flock(int(h.file.Fd()), syscall.LOCK_UN)
	closeErr := h.file.Close()
	h.file = nil
	return errors.Join(removeErr, closeErr)
}

func newReaperLeaseToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate reaper lease token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}

func reaperNameLockSetWithOwnership(name string, owned bool) ([]string, []string, []string, error) {
	legacyPath, err := rawLegacyNameLockPath(name)
	if err != nil {
		return nil, nil, nil, err
	}
	transitionalPath, err := rawTransitionalNameLockPath(name)
	if err != nil {
		return nil, nil, nil, err
	}
	statePath, err := rawNameLockPath(name)
	if err != nil {
		return nil, nil, nil, err
	}
	maintenancePath := filepath.Join(filepath.Dir(statePath), nameLockMaintenanceFile)
	rawPaths := []string{legacyPath, transitionalPath, maintenancePath, statePath}

	var token string
	if owned {
		token, err = newReaperLeaseToken()
		if err != nil {
			return nil, nil, nil, err
		}
	}

	// Cleanup takes this maintenance lock before unlinking any state lock.
	// Holding it while leases are created closes the registration/cleanup
	// race without retaining a name lock.
	leaseCtx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	maintenanceUnlock, err := acquireLockFile(leaseCtx, maintenanceNameLockStage, maintenancePath, false, nil)
	if err != nil {
		return nil, nil, nil, err
	}

	paths := make([]string, 0, len(rawPaths))
	createdFixed := make([]bool, 0, len(rawPaths))
	holds := make([]string, 0, len(rawPaths))
	fail := func(err error) ([]string, []string, []string, error) {
		maintenanceUnlock()
		if owned {
			_ = releaseReaperLeaseFilesInternal(paths, identitiesForLeasePaths(paths), holds, false, createdFixed)
		}
		return nil, nil, nil, err
	}
	for _, rawPath := range rawPaths {
		existing, existingErr := reaperLeasePaths(rawPath)
		if existingErr != nil {
			return fail(existingErr)
		}
		leasePath, leaseErr := ensureReaperLease(rawPath)
		if leaseErr != nil {
			return fail(leaseErr)
		}
		paths = append(paths, leasePath)
		createdFixed = append(createdFixed, len(existing) == 0)
		if owned {
			hold, holdErr := ensureReaperLeaseHold(rawPath, leasePath, token)
			if holdErr != nil {
				return fail(holdErr)
			}
			holds = append(holds, hold)
		}
	}
	identities := make([]string, 0, len(paths))
	for _, path := range paths {
		f, openErr := openLockFile(path, false, stateNameLockStage, nil)
		if openErr != nil {
			return fail(openErr)
		}
		info, statErr := f.Stat()
		if statErr != nil {
			_ = f.Close()
			return fail(statErr)
		}
		identity, identityErr := lockFileIdentity(info)
		closeErr := f.Close()
		if identityErr != nil {
			return fail(identityErr)
		}
		if closeErr != nil {
			return fail(closeErr)
		}
		identities = append(identities, identity)
	}
	maintenanceUnlock()
	return paths, identities, holds, nil
}

func identitiesForLeasePaths(paths []string) []string {
	identities := make([]string, 0, len(paths))
	for _, path := range paths {
		f, err := openLockFile(path, false, stateNameLockStage, nil)
		if err != nil {
			identities = append(identities, "")
			continue
		}
		info, statErr := f.Stat()
		if statErr == nil {
			identity, identityErr := lockFileIdentity(info)
			if identityErr == nil {
				identities = append(identities, identity)
			} else {
				identities = append(identities, "")
			}
		} else {
			identities = append(identities, "")
		}
		_ = f.Close()
	}
	return identities
}

// reaperNameLockPaths preserves the pre-lease helper shape for package
// callers while the reaper itself uses the complete ordered set above.
func reaperNameLockPaths(name string) (statePath, maintenancePath string, err error) {
	paths, _, err := reaperNameLockSet(name)
	if err != nil {
		return "", "", err
	}
	if len(paths) != 4 {
		return "", "", fmt.Errorf("reaper lock set has %d barriers, want 4", len(paths))
	}
	return paths[3], paths[2], nil
}

func ensureReaperLease(rawPath string) (string, error) {
	leasePath := rawPath + nameLockLeaseSuffix
	existing, err := canonicalNameLockPath(rawPath)
	if err != nil {
		return "", err
	}
	if existing != rawPath {
		if existing == leasePath {
			return existing, nil
		}
		// A previous owner left a hold but its fixed lease was collected.
		// Recreate the fixed name from the still-validated hard link.
		return createReaperLeaseFromSource(existing, leasePath)
	}

	f, err := openLockFile(rawPath, true, stateNameLockStage, nil)
	if err != nil {
		return "", err
	}
	openedInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return "", err
	}
	if err := os.Link(rawPath, leasePath); err != nil && !errors.Is(err, fs.ErrExist) {
		_ = f.Close()
		return "", fmt.Errorf("create reaper lease %s: %w", leasePath, err)
	}
	leaseFile, err := openLockFile(leasePath, true, stateNameLockStage, nil)
	if err != nil {
		_ = f.Close()
		return "", err
	}
	leaseInfo, statErr := leaseFile.Stat()
	if statErr == nil && !os.SameFile(openedInfo, leaseInfo) {
		statErr = fmt.Errorf("reaper lease %s does not match lock inode %s", leasePath, rawPath)
	}
	closeErr := leaseFile.Close()
	rawCloseErr := f.Close()
	if statErr != nil {
		return "", statErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if rawCloseErr != nil {
		return "", rawCloseErr
	}
	return leasePath, nil
}

func createReaperLeaseFromSource(source, leasePath string) (string, error) {
	f, err := openLockFile(source, false, stateNameLockStage, nil)
	if err != nil {
		return "", err
	}
	sourceInfo, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return "", statErr
	}
	if err := os.Link(source, leasePath); err != nil && !errors.Is(err, fs.ErrExist) {
		_ = f.Close()
		return "", fmt.Errorf("create reaper lease %s: %w", leasePath, err)
	}
	leaseFile, err := openLockFile(leasePath, true, stateNameLockStage, nil)
	if err != nil {
		_ = f.Close()
		return "", err
	}
	leaseInfo, leaseStatErr := leaseFile.Stat()
	if leaseStatErr == nil && !os.SameFile(sourceInfo, leaseInfo) {
		leaseStatErr = fmt.Errorf("reaper lease %s does not match source inode %s", leasePath, source)
	}
	closeErr := leaseFile.Close()
	sourceCloseErr := f.Close()
	if leaseStatErr != nil {
		return "", leaseStatErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if sourceCloseErr != nil {
		return "", sourceCloseErr
	}
	return leasePath, nil
}

func ensureReaperLeaseHold(rawPath, fixedPath, token string) (string, error) {
	if !validReaperLeaseToken(token) {
		return "", fmt.Errorf("invalid reaper lease token")
	}
	holdPath := rawPath + nameLockLeaseHoldSuffix + token
	source := fixedPath
	if _, err := os.Lstat(source); errors.Is(err, fs.ErrNotExist) {
		source = rawPath
	} else if err != nil {
		return "", err
	}
	f, err := openLockFile(source, !isReaperLeasePath(source), stateNameLockStage, nil)
	if err != nil {
		return "", err
	}
	sourceInfo, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return "", statErr
	}
	if err := os.Link(source, holdPath); err != nil && !errors.Is(err, fs.ErrExist) {
		_ = f.Close()
		return "", fmt.Errorf("create reaper lease hold %s: %w", holdPath, err)
	}
	holdFile, err := openLockFile(holdPath, true, stateNameLockStage, nil)
	if err != nil {
		_ = f.Close()
		return "", err
	}
	holdInfo, holdStatErr := holdFile.Stat()
	if holdStatErr == nil && !os.SameFile(sourceInfo, holdInfo) {
		holdStatErr = fmt.Errorf("reaper lease hold %s does not match source inode %s", holdPath, source)
	}
	closeErr := holdFile.Close()
	sourceCloseErr := f.Close()
	if holdStatErr != nil {
		return "", holdStatErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if sourceCloseErr != nil {
		return "", sourceCloseErr
	}
	return holdPath, nil
}

func rawPathFromLease(path string) (string, bool) {
	if strings.HasSuffix(path, nameLockLeaseSuffix) {
		return strings.TrimSuffix(path, nameLockLeaseSuffix), true
	}
	marker := nameLockLeaseHoldSuffix
	if i := strings.LastIndex(path, marker); i >= 0 && validReaperLeaseToken(path[i+len(marker):]) {
		return path[:i], true
	}
	return "", false
}

func removeReaperLeaseFile(path, expectedIdentity string, barriersHeld bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat reaper lease %s: %w", path, err)
	}
	if err := checkLockFile(info, path); err != nil {
		return fmt.Errorf("validate reaper lease %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open reaper lease %s: %w", path, err)
	}
	opened, statErr := f.Stat()
	if statErr == nil {
		statErr = checkOpenedLockFile(f, path)
	}
	var identity string
	if statErr == nil {
		identity, statErr = lockFileIdentity(opened)
	}
	if statErr == nil && expectedIdentity != "" && identity != expectedIdentity {
		statErr = fmt.Errorf("reaper lease %s changed inode", path)
	}
	if statErr == nil && !barriersHeld {
		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(lockErr, syscall.EWOULDBLOCK) || errors.Is(lockErr, syscall.EAGAIN) {
			statErr = fmt.Errorf("reaper lease %s is busy", path)
		} else if lockErr != nil {
			statErr = fmt.Errorf("probe reaper lease %s: %w", path, lockErr)
		}
	}
	if statErr == nil {
		current, currentErr := os.Lstat(path)
		if currentErr != nil {
			statErr = currentErr
		} else if !os.SameFile(opened, current) {
			statErr = fmt.Errorf("reaper lease %s changed while releasing", path)
		} else if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			statErr = err
		}
	}
	_ = f.Close()
	if statErr != nil {
		return statErr
	}
	return nil
}

// releaseReaperLeaseFiles releases one registration's ownership holds and
// then removes the shared fixed lease when no other hold remains. Unless
// barriersHeld is true, it acquires the maintenance barrier first; that
// barrier serializes lease creation/removal. The skip-lock flag avoids a
// self-deadlock on the maintenance lease while still checking path identity.
func releaseReaperLeaseFiles(fixedPaths, identities, holds []string, barriersHeld bool) error {
	return releaseReaperLeaseFilesInternal(fixedPaths, identities, holds, barriersHeld, nil)
}

func releaseReaperLeaseFilesInternal(fixedPaths, identities, holds []string, barriersHeld bool, removeUnowned []bool) error {
	if len(fixedPaths) == 0 {
		return nil
	}
	if len(identities) != len(fixedPaths) {
		return fmt.Errorf("reaper lease identity count = %d, want %d", len(identities), len(fixedPaths))
	}
	if len(holds) > len(fixedPaths) {
		return fmt.Errorf("reaper lease hold count = %d, exceeds %d barriers", len(holds), len(fixedPaths))
	}
	if !barriersHeld {
		statePath, ok := rawPathFromLease(fixedPaths[len(fixedPaths)-1])
		if !ok {
			return fmt.Errorf("invalid reaper state lease %q", fixedPaths[len(fixedPaths)-1])
		}
		maintenancePath, pathErr := maintenanceNameLockPath(filepath.Dir(statePath))
		if pathErr != nil {
			return pathErr
		}
		ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
		maintenanceUnlock, lockErr := acquireLockFile(ctx, maintenanceNameLockStage, maintenancePath, false, nil)
		cancel()
		if lockErr != nil {
			return lockErr
		}
		defer maintenanceUnlock()
	}

	var errs []error
	for i, hold := range holds {
		expected := ""
		if i < len(identities) {
			expected = identities[i]
		}
		if expected == "" {
			errs = append(errs, fmt.Errorf("missing identity for reaper lease hold %s", hold))
			continue
		}
		if err := removeReaperLeaseFile(hold, expected, true); err != nil {
			errs = append(errs, err)
		}
	}
	if len(holds) == 0 && len(removeUnowned) == 0 {
		// Entries made by the lease-aware registration path always carry
		// holds. Do not unlink an unowned fixed lease: an older process may
		// still be using the shared name.
		return errors.Join(errs...)
	}
	seen := make(map[string]struct{}, len(fixedPaths))
	for i, fixed := range fixedPaths {
		if _, ok := seen[fixed]; ok {
			continue
		}
		seen[fixed] = struct{}{}
		raw, ok := rawPathFromLease(fixed)
		if !ok {
			errs = append(errs, fmt.Errorf("invalid reaper lease path %q", fixed))
			continue
		}
		leases, err := reaperLeasePaths(raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		other := false
		for _, lease := range leases {
			if lease != fixed {
				other = true
				break
			}
		}
		if other {
			continue
		}
		ownedByCall := i < len(holds) || (i < len(removeUnowned) && removeUnowned[i])
		if !ownedByCall {
			continue
		}
		if identities[i] == "" {
			errs = append(errs, fmt.Errorf("missing identity for reaper lease %s", fixed))
			continue
		}
		if err := removeReaperLeaseFile(fixed, identities[i], true); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func lockFileIdentity(info os.FileInfo) (string, error) {
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() {
		if value.Kind() == reflect.Pointer {
			if value.IsNil() {
				return "", fmt.Errorf("lock file stat is nil")
			}
			value = value.Elem()
		}
		if value.Kind() == reflect.Struct {
			dev := value.FieldByName("Dev")
			ino := value.FieldByName("Ino")
			if dev.IsValid() && ino.IsValid() {
				return fmt.Sprintf("%d:%d", dev.Interface(), ino.Interface()), nil
			}
		}
	}
	return "", fmt.Errorf("lock file device/inode identity is unavailable")
}

func nameLockFileName(name string) string {
	digest := sha256.Sum256([]byte(name))
	return hex.EncodeToString(digest[:]) + ".lock"
}

// nameLockDir returns a private directory below a fixed per-user state
// root. Process environment variables such as XDG_STATE_HOME and HOME are
// deliberately not consulted: they can differ between otherwise identical
// cooperating processes and must not split the coordination namespace.
func nameLockDir() (string, error) {
	stateDir, err := userStateDir()
	if err != nil {
		return "", err
	}
	appDir, err := ensurePrivateDir(filepath.Join(stateDir, "container-go"))
	if err != nil {
		return "", err
	}
	lockDir, err := ensurePrivateDir(filepath.Join(appDir, "locks"))
	if err != nil {
		return "", err
	}
	return lockDir, nil
}

func userStateDir() (string, error) {
	if nameLockStateRootOverride != "" {
		return prepareUserBase(nameLockStateRootOverride, "user state directory")
	}
	home, err := currentUserHome()
	if err != nil {
		return "", fmt.Errorf("find durable user state directory: %w", err)
	}
	return userStateDirFromHome(home)
}

func userStateDirFromHome(home string) (string, error) {
	return prepareUserBase(defaultStateDir(home, ""), "user state directory")
}

func defaultStateDir(home, _ string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support")
	}
	return filepath.Join(home, ".local", "state")
}

// currentUserHome uses the account database exclusively. Falling back to
// HOME would allow two processes with the same account but different
// environments to select different lock namespaces, so failure to obtain
// the account home is fail-closed.
func currentUserHome() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("look up current user: %w", err)
	}
	home, err := existingUserDir(current.HomeDir)
	if err != nil {
		return "", fmt.Errorf("use account home %q: %w", current.HomeDir, err)
	}
	return home, nil
}

func existingUserDir(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path is not absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if err := ensureSecureDirPath(resolved, true); err != nil {
		return "", err
	}
	return resolved, nil
}

// transitionalNameLockPath is the namespace introduced by the first
// hardened revision. It remains a mandatory migration barrier until old
// binaries can no longer coexist with this one.
func transitionalNameLockPath(name string) (string, error) {
	raw, err := rawTransitionalNameLockPath(name)
	if err != nil {
		return "", err
	}
	return canonicalNameLockPath(raw)
}

func rawTransitionalNameLockPath(name string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find transitional user cache directory: %w", err)
	}
	cacheDir, err = prepareUserBase(cacheDir, "transitional user cache directory")
	if err != nil {
		return "", err
	}
	appDir, err := ensurePrivateDir(filepath.Join(cacheDir, "container-go"))
	if err != nil {
		return "", err
	}
	lockDir, err := ensurePrivateDir(filepath.Join(appDir, "locks"))
	if err != nil {
		return "", err
	}
	return filepath.Join(lockDir, nameLockFileName(name)), nil
}

func legacyNameLockPath(name string) (string, error) {
	raw, err := rawLegacyNameLockPath(name)
	if err != nil {
		return "", err
	}
	return canonicalNameLockPath(raw)
}

func rawLegacyNameLockPath(name string) (string, error) {
	tempDir := os.TempDir()
	// Resolve safe system symlinks such as /var -> /private/var. The old
	// binary opens the alias, while this revision opens the same final
	// directory inode.
	resolved, err := filepath.EvalSymlinks(tempDir)
	if err != nil {
		return "", fmt.Errorf("resolve legacy temporary directory %s: %w", tempDir, err)
	}
	if err := ensureSecureDirPath(resolved, false); err != nil {
		return "", fmt.Errorf("legacy temporary directory %s is not user-safe: %w", resolved, err)
	}
	return filepath.Join(resolved, "containergo-"+name+".lock"), nil
}

func maintenanceNameLockPath(stateDir string) (string, error) {
	raw := filepath.Join(stateDir, nameLockMaintenanceFile)
	return canonicalNameLockPath(raw)
}

// prepareUserBase resolves all existing path components, creates missing
// private components, and returns the canonical path. Final components
// must be owned by the current user and inaccessible for group/other
// writes; intermediate sticky directories such as /tmp are safe because
// their sticky rule prevents another user from replacing our entry.
func prepareUserBase(path, kind string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s %s is not absolute", kind, path)
	}
	canonical, err := pathForCreate(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s %s: %w", kind, path, err)
	}
	if err := ensureSecureDirPath(canonical, true); err != nil {
		return "", fmt.Errorf("prepare %s %s: %w", kind, path, err)
	}
	return canonical, nil
}

func pathForCreate(path string) (string, error) {
	path = filepath.Clean(path)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("path %s is a symbolic link", path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	var missing []string
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func ensureSecureDirPath(path string, userOwnedFinal bool) error {
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	if parent != path {
		if err := ensureSecureDirPath(parent, false); err != nil {
			return err
		}
	}
	if err := os.Mkdir(path, nameLockDirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create directory %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open directory %s without following links: %w", path, err)
	}
	info, statErr := f.Stat()
	_ = f.Close()
	if statErr != nil {
		return fmt.Errorf("stat directory %s: %w", path, statErr)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat lock directory %s: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("lock directory %s is not a directory", path)
	}
	if !os.SameFile(info, pathInfo) {
		return fmt.Errorf("lock directory %s changed while opening", path)
	}
	if err := checkNamespaceOwner(info, path); err != nil {
		return err
	}
	writable := info.Mode().Perm() & 0o022
	if writable != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("lock directory %s is writable by group or other (%04o)", path, info.Mode().Perm())
	}
	if userOwnedFinal {
		if err := checkLockOwner(info, "lock directory"); err != nil {
			return err
		}
		if writable != 0 {
			return fmt.Errorf("lock directory %s is writable by group or other (%04o)", path, info.Mode().Perm())
		}
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return fmt.Errorf("lock directory %s has special mode bits", path)
		}
	}
	return nil
}

func ensurePrivateDir(path string) (string, error) {
	parent, err := prepareUserBase(filepath.Dir(path), "private lock parent")
	if err != nil {
		return "", err
	}
	path = filepath.Join(parent, filepath.Base(path))
	if err := ensureSecureDirPath(path, true); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("stat private lock directory %s: %w", path, err)
	}
	if info.Mode().Perm() != nameLockDirPerm {
		return "", fmt.Errorf("lock directory %s has permissions %04o, want %04o", path, info.Mode().Perm(), nameLockDirPerm)
	}
	return path, nil
}

func checkNamespaceOwner(info os.FileInfo, path string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("lock directory %s ownership is unavailable", path)
	}
	uid := uint32(os.Geteuid())
	if stat.Uid != uid && stat.Uid != 0 {
		return fmt.Errorf("lock directory %s is owned by uid %d, want uid %d or root", path, stat.Uid, uid)
	}
	return nil
}

func checkLockOwner(info os.FileInfo, kind string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s ownership is unavailable", kind)
	}
	if uid := uint32(os.Geteuid()); stat.Uid != uid {
		return fmt.Errorf("%s is owned by uid %d, want uid %d", kind, stat.Uid, uid)
	}
	return nil
}

func checkLockFile(info os.FileInfo, path string) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("lock file %s is a symbolic link", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("lock file %s is not a regular file", path)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("lock file %s has special mode bits", path)
	}
	if info.Mode().Perm() != nameLockFilePerm {
		return fmt.Errorf("lock file %s has permissions %04o, want %04o", path, info.Mode().Perm(), nameLockFilePerm)
	}
	return checkLockOwner(info, "lock file")
}

// lockName serializes name-addressed creates and generation-checked
// deletes across cooperating processes on this host. Every new caller
// takes the parent revision's TMPDIR flock, the UserCacheDir flock
// introduced by the first hardened revision, namespace maintenance, and
// the durable state flock, in that order. A cooperating caller from an
// older revision can be serialized only when it uses the same historical
// barrier; an older reaper that does not take these barriers is outside
// this protocol. Failure to establish a historical namespace is a
// fail-closed compatibility error.
var errNameLockPathChanged = errors.New("name lock lease changed while acquiring")

const nameLockLeaseRetryLimit = 8

func lockName(ctx context.Context, name string) (func(), error) {
	return lockNameWithHooks(ctx, name, nil)
}

func lockNameWithHooks(ctx context.Context, name string, hooks *nameLockHooks) (func(), error) {
	var last error
	for attempt := 0; attempt < nameLockLeaseRetryLimit; attempt++ {
		unlock, err := lockNameWithHooksOnce(ctx, name, hooks)
		if err == nil {
			return unlock, nil
		}
		if !errors.Is(err, errNameLockPathChanged) {
			return nil, err
		}
		last = err
	}
	return nil, last
}

func lockNameWithHooksOnce(ctx context.Context, name string, hooks *nameLockHooks) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: invalid container name %q", ErrNameLockCompatibility, name)
	}

	// Resolve all paths before taking a barrier. In particular, a missing
	// UserCacheDir is surfaced instead of silently entering a critical
	// section that an older hardened process could also enter.
	legacyPath, err := legacyNameLockPath(name)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve legacy lock: %w", ErrNameLockCompatibility, err)
	}
	cachePath, err := transitionalNameLockPath(name)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve transitional lock: %w", ErrNameLockCompatibility, err)
	}
	statePath, err := nameLockPath(name)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve durable lock: %w", ErrNameLockCompatibility, err)
	}

	var acquired []func()
	release := func() {
		for i := len(acquired) - 1; i >= 0; i-- {
			acquired[i]()
		}
	}
	acquire := func(stage nameLockStage, path string, touch bool) error {
		unlock, err := acquireLockFile(ctx, stage, path, touch, hooks)
		if err != nil {
			return wrapNameLockAcquireError(stage, err)
		}
		acquired = append(acquired, unlock)
		return nil
	}

	// This order is a compatibility invariant. Do not reorder these calls.
	if err := acquire(legacyNameLockStage, legacyPath, false); err != nil {
		release()
		return nil, err
	}
	if err := acquire(cacheNameLockStage, cachePath, false); err != nil {
		release()
		return nil, err
	}

	stateDir := filepath.Dir(statePath)
	maintenancePath, err := maintenanceNameLockPath(stateDir)
	if err != nil {
		release()
		return nil, fmt.Errorf("%w: resolve lock maintenance: %w", ErrNameLockCompatibility, err)
	}
	maintenanceUnlock, err := acquireLockFile(ctx, maintenanceNameLockStage, maintenancePath, false, nil)
	if err != nil {
		release()
		return nil, wrapNameLockAcquireError("lock maintenance", err)
	}
	// A reaper may have created a lease while we waited for maintenance.
	// Re-resolve the durable path under that barrier so this caller never
	// opens a newly-created inode while the reaper still owns the lease.
	statePath, err = canonicalNameLockPath(statePath)
	if err != nil {
		maintenanceUnlock()
		release()
		return nil, fmt.Errorf("%w: resolve durable lock: %w", ErrNameLockCompatibility, err)
	}
	if err := acquire(stateNameLockStage, statePath, true); err != nil {
		maintenanceUnlock()
		release()
		return nil, err
	}

	// Keep maintenance held while the stale-file sweep runs. Releasing it
	// before the sweep would allow another caller to hold maintenance while
	// waiting for this state lock, producing a lock-order inversion.
	if hooks != nil && hooks.beforeCleanup != nil {
		hooks.beforeCleanup()
	}
	if err := cleanupNameLockFilesLocked(ctx, stateDir, statePath, time.Now(), nameLockCleanupScan, nameLockCleanupDelete); err != nil {
		maintenanceUnlock()
		release()
		return nil, wrapNameLockMaintenanceError(err)
	}
	maintenanceUnlock()
	return release, nil
}

func wrapNameLockAcquireError(stage nameLockStage, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("acquire %s lock: %w", stage, err)
	}
	return fmt.Errorf("%w: acquire %s lock: %w", ErrNameLockCompatibility, stage, err)
}

func wrapNameLockMaintenanceError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("lock maintenance: %w", err)
	}
	return fmt.Errorf("%w: lock maintenance: %w", ErrNameLockCompatibility, err)
}

func isReaperLeasePath(path string) bool {
	name := filepath.Base(path)
	if strings.HasSuffix(name, nameLockLeaseSuffix) {
		return true
	}
	marker := nameLockLeaseHoldSuffix
	if i := strings.Index(name, marker); i >= 0 {
		return validReaperLeaseToken(name[i+len(marker):])
	}
	return false
}

func acquireLockFile(ctx context.Context, stage nameLockStage, path string, touch bool, hooks *nameLockHooks) (func(), error) {
	create := !isReaperLeasePath(path)
	f, err := openLockFile(path, create, stage, hooks)
	if err != nil {
		if isReaperLeasePath(path) && errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %v", errNameLockPathChanged, err)
		}
		return nil, err
	}
	if err := flockWithContext(ctx, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := checkOpenedLockFile(f, path); err != nil {
		closeFileLock(f)()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %v", errNameLockPathChanged, err)
		}
		return nil, err
	}
	if touch {
		now := time.Now()
		times := []syscall.Timeval{syscall.NsecToTimeval(now.UnixNano())}
		_ = syscall.Futimes(int(f.Fd()), times)
	}
	return closeFileLock(f), nil
}

func openLockFile(path string, create bool, stage nameLockStage, hooks *nameLockHooks) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if err := checkLockFile(info, path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat lock file %s: %w", path, err)
	} else if !create {
		return nil, err
	}
	if hooks != nil && hooks.beforeOpen != nil {
		hooks.beforeOpen(stage, path)
	}

	flags := os.O_RDWR | syscall.O_NOFOLLOW
	var f *os.File
	var err error
	created := false
	if create {
		f, err = os.OpenFile(path, flags|os.O_CREATE|syscall.O_EXCL, nameLockFilePerm)
		if err == nil {
			created = true
		}
		if errors.Is(err, fs.ErrExist) {
			// Another new process won creation, or a pre-existing file was
			// validated above. Never follow a symlink installed in between.
			f, err = os.OpenFile(path, flags, 0)
		}
	} else {
		f, err = os.OpenFile(path, flags, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open lock file %s without following links: %w", path, err)
	}
	if created {
		// Restore the intended mode even if a process has an unusually
		// restrictive umask; this file was created with O_EXCL by us.
		if err := f.Chmod(nameLockFilePerm); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("set lock file permissions %s: %w", path, err)
		}
	}
	if hooks != nil && hooks.afterOpen != nil {
		hooks.afterOpen(stage, path)
	}
	if err := checkOpenedLockFile(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func checkOpenedLockFile(f *os.File, path string) error {
	opened, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat open lock file %s: %w", path, err)
	}
	if err := checkLockFile(opened, path); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("recheck lock file %s: %w", path, err)
	}
	if err := checkLockFile(current, path); err != nil {
		return err
	}
	if !os.SameFile(opened, current) {
		return fmt.Errorf("lock file %s was replaced while opening", path)
	}
	return nil
}

func flockWithContext(ctx context.Context, f *os.File) error {
	ticker := time.NewTicker(nameLockPoll)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func closeFileLock(f *os.File) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}
}

// Active-state markers are flock-held independently of lease mtimes. A live
// marker protects the recorded state-lock names for the lifetime of its
// owning reaper; the child inherits the same descriptor.

// cleanupNameLockFiles performs a bounded opportunistic sweep. It never
// waits for a name lock: a busy candidate is live and is skipped. The
// namespace maintenance lock excludes the open-then-flock window used by
// all new callers, so a candidate cannot be unlinked between those steps.
// The scan is capped at nameLockMaxFiles plus the maintenance entry; if
// that cap is exceeded, only unlocked candidates are removed.
func cleanupNameLockFiles(ctx context.Context, dir, keep string, now time.Time) error {
	return cleanupNameLockFilesAt(ctx, dir, keep, now, nameLockCleanupScan, nameLockCleanupDelete)
}

func cleanupNameLockFilesAt(ctx context.Context, dir, keep string, now time.Time, scanLimit, deleteLimit int) error {
	maintenancePath, err := maintenanceNameLockPath(dir)
	if err != nil {
		return err
	}
	maintenanceUnlock, err := acquireLockFile(ctx, maintenanceNameLockStage, maintenancePath, false, nil)
	if err != nil {
		return err
	}
	defer maintenanceUnlock()
	return cleanupNameLockFilesLocked(ctx, dir, keep, now, scanLimit, deleteLimit)
}

// cleanupNameLockFilesLocked performs the sweep while the caller holds the
// namespace maintenance lock. Keeping that lock held is what prevents a
// second caller from opening a candidate between the probe and unlink.
func cleanupNameLockFilesLocked(ctx context.Context, dir, keep string, now time.Time, scanLimit, deleteLimit int) error {
	df, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open lock directory %s: %w", dir, err)
	}
	entries, readErr := df.ReadDir(scanLimit)
	closeErr := df.Close()
	if readErr != nil {
		return fmt.Errorf("read lock directory %s: %w", dir, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close lock directory %s: %w", dir, closeErr)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	validNames := 0
	leaseProtected := make(map[string]bool)
	leaseCount := 0
	activeEntries := make([]string, 0)
	activeProtectedRaws := make(map[string]bool)
	activeProtectedAll := false
	for _, entry := range entries {
		if isNameLockFile(entry.Name()) {
			validNames++
		}
		if raw, _, ok := reaperLeaseRawName(entry.Name()); ok {
			leaseCount++
			leaseProtected[raw] = true
		}
		if _, valid, candidate := reaperActiveMarkerName(entry.Name()); candidate {
			if !valid {
				activeProtectedAll = true
				continue
			}
			path := filepath.Join(dir, entry.Name())
			activeEntries = append(activeEntries, path)
			state, stateErr := reaperActiveMarkerStateAt(path)
			if stateErr != nil {
				activeProtectedAll = true
				continue
			}
			if state.failClosed {
				activeProtectedAll = true
				continue
			}
			if state.live {
				for raw := range state.raws {
					activeProtectedRaws[raw] = true
				}
			}
		}
	}
	overLimit := len(entries) >= scanLimit || validNames > nameLockMaxFiles
	leaseOverLimit := leaseCount+len(activeEntries) > nameLockMaxLeases
	// ReadDir(n) does not report whether more entries remain. If the scan
	// may be truncated, postpone all lease reclamation: an active marker
	// outside the window must still protect its aged lease links.
	scanTruncated := len(entries) >= scanLimit
	leaseEntries := make([]string, 0, leaseCount)
	if !scanTruncated {
		for _, entry := range entries {
			if _, _, ok := reaperLeaseRawName(entry.Name()); ok {
				leaseEntries = append(leaseEntries, filepath.Join(dir, entry.Name()))
			}
		}
	}
	removed := 0
	if !scanTruncated {
		var leaseRemoved int
		var err error
		if activeProtectedAll {
			leaseRemoved, err = cleanupReaperLeaseFilesLockedWithActive(ctx, dir, keep, leaseEntries, now, leaseOverLimit, deleteLimit, true)
		} else if len(activeProtectedRaws) == 0 {
			leaseRemoved, err = cleanupReaperLeaseFilesLocked(ctx, dir, keep, leaseEntries, now, leaseOverLimit, deleteLimit)
		} else {
			leaseRemoved, err = cleanupReaperLeaseFilesLockedWithRaws(ctx, dir, keep, leaseEntries, now, leaseOverLimit, deleteLimit, activeProtectedRaws, false)
		}
		if err != nil {
			return err
		}
		removed += leaseRemoved
		for _, path := range activeEntries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if removed >= deleteLimit {
				break
			}
			info, statErr := os.Lstat(path)
			if statErr != nil {
				continue
			}
			if !overLimit && now.Sub(info.ModTime()) < nameLockRetention {
				continue
			}
			live, liveErr := reaperActiveMarkerLiveAt(path)
			if liveErr != nil || live {
				continue
			}
			if err := removeReaperLeaseFile(path, "", false); err == nil {
				removed++
			}
		}
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if removed >= deleteLimit {
			break
		}
		name := entry.Name()
		if name == filepath.Base(keep) || name == nameLockMaintenanceFile || !isNameLockFile(name) {
			continue
		}
		path := filepath.Join(dir, name)
		if leaseProtected[name] || reaperLeaseExists(path) {
			continue
		}
		f, err := openLockFile(path, false, stateNameLockStage, nil)
		if err != nil {
			// Invalid, replaced, or concurrently removed entries are not
			// removed. Retention must never turn a metadata conflict into
			// deletion of an unknown inode.
			continue
		}
		unlock := closeFileLock(f)
		flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(flockErr, syscall.EWOULDBLOCK) || errors.Is(flockErr, syscall.EAGAIN) {
			unlock()
			continue
		}
		if flockErr != nil {
			unlock()
			return fmt.Errorf("probe stale lock file %s: %w", path, flockErr)
		}
		if err := checkOpenedLockFile(f, path); err != nil {
			unlock()
			continue
		}
		info, err := f.Stat()
		if err != nil || (!overLimit && now.Sub(info.ModTime()) < nameLockRetention) {
			unlock()
			continue
		}
		// Release the probe before unlinking. The maintenance lock keeps
		// another new caller from opening this path, so the inode is not
		// live at the instant it is removed.
		unlock()
		current, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("recheck stale lock file %s: %w", path, err)
		}
		if !os.SameFile(info, current) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove stale lock file %s: %w", path, err)
		}
		removed++
	}
	return nil
}

func reaperLeaseRawName(name string) (raw string, fixed bool, ok bool) {
	if strings.HasSuffix(name, nameLockLeaseSuffix) {
		raw = strings.TrimSuffix(name, nameLockLeaseSuffix)
		if strings.HasSuffix(raw, ".lock") {
			return raw, true, true
		}
	}
	marker := nameLockLeaseHoldSuffix
	if i := strings.Index(name, marker); i > 0 && validReaperLeaseToken(name[i+len(marker):]) {
		raw = name[:i]
		if strings.HasSuffix(raw, ".lock") {
			return raw, false, true
		}
	}
	return "", false, false
}

// reaperActiveMarkerName recognizes the process-level marker names. A
// marker-shaped name with an invalid token is still a candidate so GC can
// fail closed for the whole directory.
func reaperActiveMarkerName(name string) (token string, valid bool, candidate bool) {
	prefix := nameLockActiveSuffix + "."
	if !strings.HasPrefix(name, prefix) {
		return "", false, false
	}
	token = strings.TrimPrefix(name, prefix)
	return token, validReaperLeaseToken(token), true
}

type reaperActiveMarkerState struct {
	raws       map[string]bool
	failClosed bool
	live       bool
}

// reaperActiveMarkerStateAt validates the marker's checksummed registration
// set before reporting its liveness; a partial or malformed marker fails
// closed rather than authorizing lease reclamation.
func reaperActiveMarkerStateAt(path string) (reaperActiveMarkerState, error) {
	state := reaperActiveMarkerState{raws: make(map[string]bool)}
	info, err := os.Lstat(path)
	if err != nil {
		state.failClosed = true
		return state, err
	}
	if err := checkLockFile(info, path); err != nil {
		state.failClosed = true
		return state, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		state.failClosed = true
		return state, err
	}
	opened, statErr := f.Stat()
	if statErr == nil {
		statErr = checkOpenedLockFile(f, path)
	}
	if statErr == nil && !os.SameFile(info, opened) {
		statErr = fmt.Errorf("reaper active marker %s changed while opening", path)
	}
	if statErr != nil {
		_ = f.Close()
		state.failClosed = true
		return state, statErr
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		state.failClosed = true
		return state, err
	}
	contents, err := io.ReadAll(f)
	if err != nil {
		_ = f.Close()
		state.failClosed = true
		return state, err
	}
	markerContents := string(contents)
	headerEnd := strings.IndexByte(markerContents, '\n')
	if headerEnd <= 0 {
		state.failClosed = true
	} else {
		header := strings.Fields(markerContents[:headerEnd])
		body := markerContents[headerEnd+1:]
		if len(header) != 3 || header[0] != "v1" {
			state.failClosed = true
		} else if count, parseErr := strconv.Atoi(header[1]); parseErr != nil || count < 0 {
			state.failClosed = true
		} else {
			checksum := sha256.Sum256([]byte(body))
			if header[2] != hex.EncodeToString(checksum[:]) {
				state.failClosed = true
			} else {
				lines := []string{}
				if body != "" {
					lines = strings.Split(body, "\n")
				}
				if len(lines) != count {
					state.failClosed = true
				} else {
					for _, raw := range lines {
						if !isNameLockFile(raw) {
							state.failClosed = true
							break
						}
						if state.raws[raw] {
							state.failClosed = true
							break
						}
						state.raws[raw] = true
					}
				}
			}
		}
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			return state, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			state.live = true
			return state, nil
		}
		state.failClosed = true
		return state, fmt.Errorf("probe reaper active marker %s: %w", path, err)
	}
}

func reaperActiveMarkerLiveAt(path string) (bool, error) {
	state, err := reaperActiveMarkerStateAt(path)
	if err != nil {
		return false, err
	}
	return state.live, nil
}

// cleanupReaperLeaseFilesLocked preserves the package-local helper shape;
// the wrapper also performs a complete active-marker probe for callers that
// do not have the bounded sweep's active-state result.
func cleanupReaperLeaseFilesLocked(ctx context.Context, dir, keep string, paths []string, now time.Time, overLimit bool, deleteLimit int) (int, error) {
	raws, all := reaperActiveMarkerProtection(dir)
	return cleanupReaperLeaseFilesLockedWithRaws(ctx, dir, keep, paths, now, overLimit, deleteLimit, raws, all)
}

func cleanupReaperLeaseFilesLockedWithActive(ctx context.Context, dir, keep string, paths []string, now time.Time, overLimit bool, deleteLimit int, activeProtected bool) (int, error) {
	return cleanupReaperLeaseFilesLockedWithRaws(ctx, dir, keep, paths, now, overLimit, deleteLimit, nil, activeProtected)
}

func cleanupReaperLeaseFilesLockedWithRaws(ctx context.Context, dir, keep string, paths []string, now time.Time, overLimit bool, deleteLimit int, activeRaws map[string]bool, activeAll bool) (int, error) {
	removed := 0
	ownedRaws := make(map[string]bool)
	for _, path := range paths {
		if raw, fixed, ok := reaperLeaseRawName(filepath.Base(path)); ok && !fixed {
			ownedRaws[raw] = true
		}
	}
	// Remove ownership holds first. A fixed lease is retained while any
	// hold remains, which makes concurrent reapers independent.
	for _, pass := range []bool{false, true} {
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return removed, err
			}
			if removed >= deleteLimit {
				return removed, nil
			}
			raw, fixed, ok := reaperLeaseRawName(filepath.Base(path))
			if !ok || fixed != pass || activeAll || activeRaws[raw] {
				continue
			}
			if path == keep || filepath.Base(path) == filepath.Base(keep) {
				continue
			}
			info, err := os.Lstat(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				continue
			}
			if err := checkLockFile(info, path); err != nil {
				continue
			}
			// A lease is a live safety reference, not an ordinary stale
			// cache entry. Age controls reclamation; the cap may accelerate
			// only an orphaned fixed link, never a recent ownership hold.
			recent := now.Sub(info.ModTime()) < nameLockRetention
			if recent && (!overLimit || !fixed) {
				continue
			}
			// Validate the complete lease set before unlinking. A malformed
			// or replaced sibling keeps the raw inode fail-closed.
			leases, err := reaperLeasePaths(filepath.Join(dir, raw))
			if err != nil {
				continue
			}
			if pass {
				other := false
				for _, lease := range leases {
					if lease != path {
						other = true
						break
					}
				}
				if other {
					continue
				}
				// A fixed lease with no ownership hold may belong to a
				// package-level preparation helper or an older process.
				// Keep it while the raw coordination inode still exists;
				// explicit reaper unregister/exit owns the normal removal
				// path. A fixed link that did have an ownership hold is
				// collectible after that hold ages out, which prevents a
				// crashed reaper from pinning the namespace forever.
				if _, rawErr := os.Lstat(filepath.Join(dir, raw)); !errors.Is(rawErr, fs.ErrNotExist) && !ownedRaws[raw] {
					continue
				}
			}
			if err := removeReaperLeaseFile(path, "", false); err != nil {
				continue
			}
			removed++
		}
	}
	return removed, nil
}

func reaperActiveMarkerProtection(dir string) (map[string]bool, bool) {
	raws := make(map[string]bool)
	all := false
	entries, err := os.ReadDir(dir)
	if err != nil {
		return raws, true
	}
	for _, entry := range entries {
		_, valid, candidate := reaperActiveMarkerName(entry.Name())
		if !candidate {
			continue
		}
		if !valid {
			all = true
			continue
		}
		state, stateErr := reaperActiveMarkerStateAt(filepath.Join(dir, entry.Name()))
		if stateErr != nil {
			all = true
			continue
		}
		if state.failClosed {
			all = true
			continue
		}
		if !state.live {
			continue
		}
		for raw := range state.raws {
			raws[raw] = true
		}
	}
	return raws, all
}

func reaperActiveMarkerProtectsRaw(dir, raw string) bool {
	raws, all := reaperActiveMarkerProtection(dir)
	return all || raws[raw]
}

func reaperLeaseExists(path string) bool {
	leases, err := reaperLeasePaths(path)
	if err != nil || len(leases) != 0 {
		return true
	}
	return reaperActiveMarkerProtectsRaw(filepath.Dir(path), filepath.Base(path))
}

func isNameLockFile(name string) bool {
	if !strings.HasSuffix(name, ".lock") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimSuffix(name, ".lock"))
	return err == nil && len(decoded) == sha256.Size
}
