package container

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type pruneQueryDeadlineRunner struct {
	*fakeRunner
	eng       engine
	deadlines map[string]time.Time
	group     string
}

func (r *pruneQueryDeadlineRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	op := args[0]
	if op == "ps" || op == "ls" || op == "inspect" || op == "rm" || op == "delete" {
		r.deadlines[op], _ = ctx.Deadline()
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
				{name: "overall cap", client: 2 * time.Minute, overall: time.Second, want: time.Second},
			} {
				t.Run(fmt.Sprintf("%s/group=%v/%s", eng.name(), grouped, tc.name), func(t *testing.T) {
					r := &pruneQueryDeadlineRunner{fakeRunner: newTestRunner(), eng: eng, deadlines: map[string]time.Time{}, group: "timeout-group"}
					clientOpts := []ClientOption{WithClientBackend(eng.name()), WithClientRunner(r)}
					if eng.name() == "docker" {
						clientOpts = append(clientOpts, WithClientDockerHost("tcp://timeout-daemon:2375"))
					}
					if tc.client > 0 {
						clientOpts = append(clientOpts, WithClientQueryTimeout(tc.client))
					}
					client, err := NewClient(clientOpts...)
					if err != nil {
						t.Fatal(err)
					}
					pruneOpts := []PruneOption{WithPruneTimeout(tc.overall)}
					if tc.query > 0 {
						pruneOpts = append(pruneOpts, WithPruneQueryTimeout(tc.query))
					}
					started := time.Now()
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
						got := d.Sub(started)
						if got < tc.want-time.Second/2 || got > tc.want+time.Second/2 {
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
			r := &pruneQueryDeadlineRunner{fakeRunner: newTestRunner(), eng: eng, deadlines: map[string]time.Time{}, group: "timeout-group"}
			started := time.Now()
			removed, err := pruneWith(context.Background(), r, eng, WithPruneQueryTimeout(2*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if len(removed) != 1 {
				t.Fatalf("removed=%v, want one candidate", removed)
			}
			for op, deadline := range r.deadlines {
				if got := deadline.Sub(started); got < 119*time.Second || got > 121*time.Second {
					t.Errorf("%s deadline=%v, want two minutes", op, got)
				}
			}
		})
	}
}

func TestClientQueryTimeoutAppliesToRun(t *testing.T) {
	r := newTestRunner()
	client, err := NewClient(WithClientBackend("apple"), WithClientRunner(r), WithClientQueryTimeout(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
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
