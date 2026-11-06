package container

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type pruneQueryDeadlineRunner struct {
	*fakeRunner
	eng       engine
	deadlines map[string]time.Time
	remaining map[string]time.Duration
	group     string
}

func (r *pruneQueryDeadlineRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	op := args[0]
	if op == "ps" || op == "ls" || op == "inspect" || op == "rm" || op == "delete" {
		r.deadlines[op], _ = ctx.Deadline()
		if r.remaining != nil {
			r.remaining[op] = time.Until(r.deadlines[op])
		}
		if op == "ps" {
			return []byte(cleanupDockerIDOne + "\n"), nil, nil
		}
		if op == "ls" || op == "inspect" {
			if r.eng.name() == "docker" {
				return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Status":"exited"},"Config":{"Labels":{%q:"true",%q:"true",%q:"0123456789abcdef",%q:%q}}}]`, cleanupDockerIDOne, managedLabel, reuseLabel, creationLabel, reuseGroupLabel, r.group)), nil, nil
			}
			return []byte(fmt.Sprintf(`[{"id":"prune-timeout-candidate","configuration":{"labels":{%q:"true",%q:"true",%q:"0123456789abcdef",%q:%q}},"status":{"state":"stopped"}}]`, managedLabel, reuseLabel, creationLabel, reuseGroupLabel, r.group)), nil, nil
		}
		return nil, nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

func TestClientPruneQueryTimeout(t *testing.T) {
	for _, eng := range []engine{dockerEngine{}, appleEngine{}} {
		for _, grouped := range []bool{false, true} {
			for _, tc := range []struct {
				name                         string
				client, query, overall, want time.Duration
			}{
				{name: "default", overall: time.Hour, want: queryTimeout},
				{name: "client short", client: 5 * time.Second, overall: time.Hour, want: 5 * time.Second},
				{name: "client long", client: 2 * time.Minute, overall: time.Hour, want: 2 * time.Minute},
				{name: "override", client: 5 * time.Second, query: 3 * time.Minute, overall: time.Hour, want: 3 * time.Minute},
				{name: "overall cap", client: 2 * time.Hour, overall: time.Hour, want: time.Hour},
			} {
				t.Run(fmt.Sprintf("%s/group=%v/%s", eng.name(), grouped, tc.name), func(t *testing.T) {
					r := &pruneQueryDeadlineRunner{fakeRunner: newTestRunner(), eng: eng, deadlines: map[string]time.Time{}, remaining: map[string]time.Duration{}, group: "timeout-group"}
					clientOpts := []ClientOption{WithClientBackend(eng.name()), WithClientRunner(r)}
					if eng.name() == "docker" {
						clientOpts = append(clientOpts, WithClientDockerHost("tcp://timeout-daemon:2375"))
					}
					if tc.client > 0 {
						clientOpts = append(clientOpts, WithClientQueryTimeout(tc.client))
					}
					var client *Client
					var err error
					if eng.name() == "docker" {
						client, err = NewClient(clientOpts...)
						if err != nil {
							t.Fatal(err)
						}
					} else {
						cfg := &clientConfig{}
						for _, opt := range clientOpts {
							if err := opt(cfg); err != nil {
								t.Fatal(err)
							}
						}
						client = &Client{eng: eng, runner: cfg.runner, queryTimeout: cfg.queryTimeout}
					}
					pruneOpts := []PruneOption{WithPruneTimeout(tc.overall)}
					if tc.query > 0 {
						pruneOpts = append(pruneOpts, WithPruneQueryTimeout(tc.query))
					}
					var removed []string
					if grouped {
						removed, err = client.PruneReuseGroup(context.Background(), r.group, pruneOpts...)
					} else {
						removed, err = client.Prune(context.Background(), pruneOpts...)
					}
					if err != nil {
						t.Fatal(err)
					}
					if len(removed) != 1 {
						t.Fatalf("removed=%v, want one candidate", removed)
					}
					list, del := "ps", "rm"
					if eng.name() == "apple" {
						list, del = "ls", "delete"
					}
					for _, op := range []string{list, "inspect", del} {
						d, ok := r.deadlines[op]
						if !ok {
							t.Fatalf("missing %s", op)
						}
						got := r.remaining[op]
						if tc.name == "overall cap" && !d.Equal(r.deadlines[list]) {
							t.Errorf("%s deadline %v exceeds shared overall deadline %v", op, d, r.deadlines[list])
						}
						if got <= tc.want/2 || got > tc.want {
							t.Errorf("%s deadline=%v, want %v", op, got, tc.want)
						}
					}
				})
			}
		}
	}
}

func TestPruneQueryTimeoutValidation(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if _, err := parsePruneOptions("Prune", []PruneOption{WithPruneQueryTimeout(d)}); err == nil {
			t.Errorf("WithPruneQueryTimeout(%v) accepted", d)
		}
		if _, err := NewClient(WithClientQueryTimeout(d)); err == nil {
			t.Errorf("WithClientQueryTimeout(%v) accepted", d)
		}
	}
}

func TestPruneQueryTimeoutWithoutClient(t *testing.T) {
	for _, eng := range []engine{dockerEngine{}, appleEngine{}} {
		t.Run(eng.name(), func(t *testing.T) {
			r := &pruneQueryDeadlineRunner{fakeRunner: newTestRunner(), eng: eng, deadlines: map[string]time.Time{}, remaining: map[string]time.Duration{}, group: "timeout-group"}
			removed, err := pruneWith(context.Background(), r, eng, WithPruneQueryTimeout(2*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if len(removed) != 1 {
				t.Fatalf("removed=%v, want one candidate", removed)
			}
			for op, got := range r.remaining {
				if got <= time.Minute || got > 2*time.Minute {
					t.Errorf("%s deadline=%v, want two minutes", op, got)
				}
			}
		})
	}
}

func TestClientQueryTimeoutAppliesToRun(t *testing.T) {
	r := newTestRunner()
	cfg := &clientConfig{}
	if err := WithClientQueryTimeout(2 * time.Minute)(cfg); err != nil {
		t.Fatal(err)
	}
	client := &Client{eng: appleEngine{}, runner: r, queryTimeout: cfg.queryTimeout}
	for _, override := range []time.Duration{0, 5 * time.Second} {
		var opts []Option
		want := 2 * time.Minute
		if override > 0 {
			opts = append(opts, WithQueryTimeout(override))
			want = override
		}
		ctr, err := client.Run(context.Background(), "redis:7-alpine", opts...)
		if err != nil {
			t.Fatal(err)
		}
		if got := ctr.effectiveQueryTimeout(); got != want {
			t.Errorf("container query timeout=%v, want %v", got, want)
		}
	}
}

func TestClientRunQueriesHonorShortTimeoutWithinRunBudget(t *testing.T) {
	f := newTestRunner()
	var remaining time.Duration
	var inspectRemaining time.Duration
	r := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if len(args) > 1 && args[0] == "image" && args[1] == "inspect" {
			deadline, _ := ctx.Deadline()
			remaining = time.Until(deadline)
		}
		if args[0] == "inspect" {
			deadline, _ := ctx.Deadline()
			inspectRemaining = time.Until(deadline)
		}
		return f.Run(ctx, args...)
	})
	client := &Client{eng: appleEngine{}, runner: r, queryTimeout: 5 * time.Second}
	ctr, err := client.Run(context.Background(), "redis:7-alpine")
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if _, err := ctr.State(parent); err != nil {
		t.Fatal(err)
	}
	if inspectRemaining <= 0 || inspectRemaining > 5*time.Second {
		t.Fatalf("handle inspect has %v remaining, want <=5s", inspectRemaining)
	}
	if remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("image inspect has %v remaining, want <=5s", remaining)
	}
}

func TestClientConstructionHonorsQueryTimeout(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	var remaining time.Duration
	r := timeoutTestRunner(func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		if args[0] == "context" {
			deadline, _ := ctx.Deadline()
			remaining = time.Until(deadline)
			return []byte(`[{"Name":"test","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`), nil, nil
		}
		return nil, nil, nil
	})
	if _, err := NewClient(WithClientBackend("docker"), WithClientRunner(r), WithClientQueryTimeout(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("context inspect has %v remaining, want <=5s", remaining)
	}
}

func TestClientConstructionQueryDeadlineIsNotIgnored(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	r := timeoutTestRunner(func(ctx context.Context, _ ...string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	_, err := NewClient(WithClientBackend("docker"), WithClientRunner(r), WithClientQueryTimeout(time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
}

type cancelAfterPruneDeleteRunner struct {
	*pruneQueryDeadlineRunner
	cancel context.CancelFunc
}

func (r cancelAfterPruneDeleteRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	stdout, stderr, err := r.pruneQueryDeadlineRunner.Run(ctx, args...)
	if args[0] == "rm" || args[0] == "delete" {
		r.cancel()
	}
	return stdout, stderr, err
}
func TestPruneKeepsSuccessfulDeleteAfterCancellation(t *testing.T) {
	for _, eng := range []engine{dockerEngine{}, appleEngine{}} {
		t.Run(eng.name(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := cancelAfterPruneDeleteRunner{pruneQueryDeadlineRunner: &pruneQueryDeadlineRunner{fakeRunner: newTestRunner(), eng: eng, deadlines: map[string]time.Time{}, group: "timeout-group"}, cancel: cancel}
			removed, err := pruneReuseGroupWith(ctx, r, eng, "timeout-group")
			if err != nil || len(removed) != 1 {
				t.Fatalf("removed=%v, err=%v, want successful removal", removed, err)
			}
		})
	}
}
