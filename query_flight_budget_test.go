package container

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCreatedOwnershipRetriesPerQueryDeadline(t *testing.T) {
	f := newTestRunner()
	ctr := runTestContainer(t, f)
	ctr.queryTimeout = 10 * time.Millisecond
	attempts := 0
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if args[0] == "inspect" {
			attempts++
			if attempts == 1 {
				<-ctx.Done()
				return nil, nil, ctx.Err()
			}
		}
		return f.Run(ctx, args...)
	})
	ctr.runner = runner
	cfg := newConfig()
	cfg.eng = appleEngine{}
	cfg.runner = runner
	cfg.queryTimeout = ctr.queryTimeout
	cfg.reusePollInterval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := verifyCreatedOwnership(ctx, ctr, cfg); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d, want2", attempts)
	}
}

func TestImageResolutionSeparatesQueryBudgets(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	started := make(chan struct{})
	var once sync.Once
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if len(args) > 1 && args[0] == "image" && args[1] == "inspect" {
			once.Do(func() { close(started) })
			select {
			case <-time.After(60 * time.Millisecond):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		return f.Run(ctx, args...)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	configFor := func(d time.Duration) *config {
		c := newConfig()
		c.eng = appleEngine{}
		c.runner = runner
		c.queryTimeout = d
		return c
	}
	short, long := configFor(10*time.Millisecond), configFor(time.Second)
	shortErr := make(chan error, 1)
	go func() { _, err := short.ensureImageRef(ctx, "redis:7-alpine"); shortErr <- err }()
	<-started
	_, longErr := long.ensureImageRef(ctx, "redis:7-alpine")
	if longErr != nil {
		t.Fatalf("healthy caller inherited short query failure: %v", longErr)
	}
	if err := <-shortErr; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short caller error=%v", err)
	}
}

func TestDockerMetadataSeparatesQueryBudgets(t *testing.T) {
	for _, kind := range []string{"network", "version"} {
		t.Run(kind, func(t *testing.T) {
			cache := newDockerMetadataCache(time.Minute)
			started := make(chan struct{})
			var once sync.Once
			runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
				if args[0] != "version" {
					t.Fatalf("unexpected command %v", args)
				}
				once.Do(func() { close(started) })
				select {
				case <-time.After(60 * time.Millisecond):
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}
				if strings.Contains(strings.Join(args, " "), "Server.Os") {
					return []byte("linux"), nil, nil
				}
				return []byte(defaultTestDockerVersion), nil, nil
			})
			query := func(ctx context.Context, d time.Duration) error {
				timeouts := opTimeouts{query: d}
				if kind == "network" {
					_, err := dockerDefaultNetworkCached(ctx, cache, runner, dockerEngine{}, timeouts)
					return err
				}
				return dockerCopyFileFromContainerVersionCached(ctx, cache, runner, dockerEngine{}, timeouts)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			shortErr := make(chan error, 1)
			go func() { shortErr <- query(ctx, 10*time.Millisecond) }()
			<-started
			if err := query(ctx, time.Second); err != nil {
				t.Fatalf("healthy caller inherited short query failure: %v", err)
			}
			if err := <-shortErr; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("short caller error=%v", err)
			}
		})
	}
}

func TestDockerMetadataHonorsLongQueryBudget(t *testing.T) {
	cache := newDockerMetadataCache(time.Minute)
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		deadline, _ := ctx.Deadline()
		if remaining := time.Until(deadline); remaining < time.Minute {
			t.Errorf("query budget=%v, want longer than fixed30s", remaining)
		}
		return []byte("linux"), nil, nil
	})
	if _, err := dockerDefaultNetworkCached(context.Background(), cache, runner, dockerEngine{}, opTimeouts{query: 2 * time.Minute}); err != nil {
		t.Fatal(err)
	}
}

func TestReuseAttachRetriesPerQueryDeadline(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	attempts := 0
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if args[0] == "inspect" {
			attempts++
			if attempts == 1 {
				<-ctx.Done()
				return nil, nil, ctx.Err()
			}
			return []byte(reuseInspectJSON("myctr", "running", "redis:7-alpine")), nil, nil
		}
		return f.Run(ctx, args...)
	})
	cfg := newConfig()
	cfg.eng = appleEngine{}
	cfg.runner = runner
	cfg.name = "myctr"
	cfg.queryTimeout = 10 * time.Millisecond
	cfg.reusePollInterval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := reuseEnsureContainer(ctx, "redis:7-alpine", cfg); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d, want2", attempts)
	}
}

func TestReuseCreatedInspectRetriesPerQueryDeadline(t *testing.T) {
	f := newReuseCreateRunner()
	attempts := 0
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if args[0] == "inspect" && f.created.Load() {
			attempts++
			if attempts == 1 {
				<-ctx.Done()
				return nil, nil, ctx.Err()
			}
		}
		return f.Run(ctx, args...)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctr, err := Run(ctx, "redis:7-alpine", WithName("myctr"), WithReuse(), WithQueryTimeout(10*time.Millisecond), WithReusePollInterval(time.Millisecond), withEngine(appleEngine{}), withRunner(runner))
	if err != nil || ctr == nil {
		t.Fatalf("Run=%v,%v", ctr, err)
	}
	if countRunnerCalls(f.fakeRunner, "delete") != 0 {
		t.Fatal("retryable query timeout rolled back a successful create")
	}
}

func TestReuseEnsureSeparatesQueryBudgets(t *testing.T) {
	f := newTestRunner()
	f.imagePresent = true
	started := make(chan struct{})
	var once sync.Once
	runner := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if args[0] == "inspect" {
			once.Do(func() { close(started) })
			select {
			case <-time.After(60 * time.Millisecond):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			return []byte(reuseInspectJSON("myctr", "running", "redis:7-alpine")), nil, nil
		}
		return f.Run(ctx, args...)
	})
	options := []Option{WithName("myctr"), WithReuse(), WithReusePollInterval(time.Millisecond), withEngine(appleEngine{}), withRunner(runner)}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	shortErr := make(chan error, 1)
	go func() {
		opts := append(append([]Option{}, options...), WithQueryTimeout(10*time.Millisecond), WithReuseAttachTimeout(100*time.Millisecond))
		_, err := Run(ctx, "redis:7-alpine", opts...)
		shortErr <- err
	}()
	<-started
	opts := append(append([]Option{}, options...), WithQueryTimeout(time.Second))
	if _, err := Run(ctx, "redis:7-alpine", opts...); err != nil {
		t.Fatalf("healthy reuse caller inherited short leader failure: %v", err)
	}
	if err := <-shortErr; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short caller error=%v", err)
	}
}

func TestDockerMetadataRejectsExpiredSuccessfulQuery(t *testing.T) {
	for _, cache := range []*dockerMetadataCache{nil, newDockerMetadataCache(time.Minute)} {
		runner := timeoutTestRunner(func(ctx context.Context, _ ...string) ([]byte, []byte, error) {
			<-ctx.Done()
			return []byte("linux"), nil, nil
		})
		_, err := dockerDefaultNetworkCached(context.Background(), cache, runner, dockerEngine{}, opTimeouts{query: time.Millisecond})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired query returned %v, want DeadlineExceeded", err)
		}
	}
}

func TestInspectQueryRetryPreservesTerminalErrorPriority(t *testing.T) {
	for name, err := range map[string]error{
		"stopped backend":     errors.Join(ErrSystemNotRunning, context.DeadlineExceeded),
		"caller cancellation": errors.Join(context.Canceled, context.DeadlineExceeded),
		"permission":          errors.Join(os.ErrPermission, context.DeadlineExceeded),
		"config":              errors.Join(&ConfigError{}, context.DeadlineExceeded),
		"TLS":                 errors.Join(errors.New("TLS certificate verification failed"), context.DeadlineExceeded),
	} {
		t.Run(name, func(t *testing.T) {
			if retryableInspectError(context.Background(), err) {
				t.Fatalf("terminal error became retryable: %v", err)
			}
		})
	}
	if !retryableInspectError(context.Background(), context.DeadlineExceeded) {
		t.Fatal("live operation did not retry its query deadline")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if retryableInspectError(ctx, context.DeadlineExceeded) {
		t.Fatal("finished operation retried its query deadline")
	}
}
