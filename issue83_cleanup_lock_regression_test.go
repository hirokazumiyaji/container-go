//go:build !windows

package container

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type cleanupLockDockerRunner struct {
	*fakeRunner
	inspectJSON string
	inspects    []string
	deletes     []string
}

func (r *cleanupLockDockerRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	switch args[0] {
	case "inspect":
		r.inspects = append(r.inspects, args[len(args)-1])
		return []byte(r.inspectJSON), nil, nil
	case "rm", "delete":
		r.deletes = append(r.deletes, args[len(args)-1])
		return nil, nil, nil
	default:
		return r.fakeRunner.Run(ctx, args...)
	}
}

func cleanupLockDockerInspect(uid, name, creation string, reuse bool) string {
	labels := map[string]string{
		managedLabel:  "true",
		sessionLabel:  sessionID(),
		creationLabel: creation,
	}
	if reuse {
		labels[reuseLabel] = "true"
	}
	data, err := json.Marshal([]map[string]any{{
		"Id":   uid,
		"Name": "/" + name,
		"State": map[string]string{
			"Status": "exited",
		},
		"Config": map[string]any{
			"Image":  "redis:7-alpine",
			"Labels": labels,
		},
	}})
	if err != nil {
		panic(err)
	}
	return string(data)
}

func configureUnusableNameCache(t *testing.T) {
	t.Helper()
	oldOverride := nameLockStateRootOverride
	nameLockStateRootOverride = t.TempDir()
	t.Cleanup(func() { nameLockStateRootOverride = oldOverride })

	cacheDir := t.TempDir()
	if err := os.Chmod(cacheDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cacheDir, 0o700) })
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", cacheDir)
}

func TestDockerCleanupDoesNotDependOnNameLockCache(t *testing.T) {
	const (
		uid      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		creation = "aaaaaaaaaaaaaaaa"
	)
	configureUnusableNameCache(t)

	t.Run("failed-create", func(t *testing.T) {
		const name = "docker-cleanup-lock"
		runner := &cleanupLockDockerRunner{
			fakeRunner:  newTestRunner(),
			inspectJSON: cleanupLockDockerInspect(uid, name, creation, false),
		}
		cfg := &config{
			runner:   runner,
			eng:      dockerEngine{},
			name:     name,
			creation: creation,
		}
		if err := cleanupFailedCreate(context.Background(), cfg, errors.New("run failed"), errors.New("run failed")); err != nil {
			t.Fatalf("cleanupFailedCreate: %v", err)
		}
		if len(runner.deletes) != 1 || runner.deletes[0] != uid {
			t.Fatalf("deletes = %v, want immutable UID %s", runner.deletes, uid)
		}
		if len(runner.inspects) != 1 || runner.inspects[0] != name {
			t.Fatalf("inspects = %v, want fresh name inspect for failed create", runner.inspects)
		}
	})

	t.Run("stopped-reuse", func(t *testing.T) {
		const name = "docker-reuse-lock"
		runner := &cleanupLockDockerRunner{
			fakeRunner:  newTestRunner(),
			inspectJSON: cleanupLockDockerInspect(uid, name, creation, true),
		}
		cfg := &config{
			runner:   runner,
			eng:      dockerEngine{},
			name:     name,
			creation: creation,
			reuse:    true,
		}
		info := &engineInfo{
			uid:   uid,
			state: StateStopped,
			labels: map[string]string{
				managedLabel:  "true",
				reuseLabel:    "true",
				creationLabel: creation,
			},
		}
		if err := deleteStoppedReuse(context.Background(), cfg, info); err != nil {
			t.Fatalf("deleteStoppedReuse: %v", err)
		}
		if len(runner.deletes) != 1 || runner.deletes[0] != uid {
			t.Fatalf("deletes = %v, want immutable UID %s", runner.deletes, uid)
		}
		if len(runner.inspects) != 1 || runner.inspects[0] != uid {
			t.Fatalf("inspects = %v, want fresh immutable-UID inspect", runner.inspects)
		}
	})
}
