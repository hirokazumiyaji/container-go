package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/wait"
)

func review91OwnedDockerInspect(uid, generation string, reused, bound bool) string {
	ports := `{}`
	if bound {
		ports = `{"6379/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}`
	}
	reuseLabelJSON := ""
	if reused {
		reuseLabelJSON = fmt.Sprintf(`,"%s":"true"`, reuseLabel)
	}
	return fmt.Sprintf(`[{
		"Id":%q,
		"Name":"/myctr",
		"Config":{"Image":"redis:7-alpine","Labels":{
			"%s":"true",
			"%s":%q,
			"%s":%q%s
		}},
		"State":{"Status":"running"},
		"NetworkSettings":{"Ports":%s}
	}]`, uid, managedLabel, sessionLabel, sessionID(), creationLabel, generation, reuseLabelJSON, ports)
}

type review91MutableInspectRunner struct {
	*fakeRunner
	mu   sync.Mutex
	data string
}

func (r *review91MutableInspectRunner) setInspect(data string) {
	r.mu.Lock()
	r.data = data
	r.mu.Unlock()
}

func (r *review91MutableInspectRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		r.mu.Lock()
		data := r.data
		r.mu.Unlock()
		return []byte(data), nil, nil
	}
	return r.fakeRunner.Run(ctx, args...)
}

type review91AfterWaitStrategy func()

func (s review91AfterWaitStrategy) WaitUntilReady(context.Context, wait.Target) error {
	s()
	return nil
}

func TestReview91ReuseRevalidatesAfterReadiness(t *testing.T) {
	oldPoll, oldTimeout := reusePollInterval, reuseAttachTimeout
	reusePollInterval = time.Millisecond
	reuseAttachTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		reusePollInterval, reuseAttachTimeout = oldPoll, oldTimeout
	})

	initial := review91OwnedDockerInspect("uid-a", "generation-a", true, true)
	cases := map[string]struct {
		after  string
		check  func(error) bool
		reason string
	}{
		"identity": {
			after:  review91OwnedDockerInspect("uid-b", "generation-b", true, true),
			check:  func(err error) bool { return errors.Is(err, ErrGenerationReplaced) },
			reason: "replacement identity",
		},
		"ownership": {
			after:  review91OwnedDockerInspect("uid-a", "generation-a", false, true),
			check:  func(err error) bool { return err != nil && strings.Contains(err.Error(), "WithReuse") },
			reason: "missing ownership",
		},
		"compatibility": {
			after:  review91OwnedDockerInspect("uid-a", "generation-a", true, false),
			check:  func(err error) bool { return err != nil && strings.Contains(err.Error(), "exposed port") },
			reason: "missing published endpoint",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &review91MutableInspectRunner{fakeRunner: newTestRunner(), data: initial}
			runner.imagePresent = true
			strategy := review91AfterWaitStrategy(func() { runner.setInspect(tc.after) })
			ctr, err := Run(context.Background(), "redis:7-alpine",
				WithName("myctr"), WithReuse(), WithExposedPorts("6379/tcp"),
				WithWaitStrategy(strategy), withRunner(runner), withEngine(dockerEngine{}))
			if err == nil || !tc.check(err) {
				if ctr != nil {
					_ = ctr.Terminate(context.Background())
				}
				t.Fatalf("Run returned container %v with error %v, want %s failure", ctr, err, tc.reason)
			}
		})
	}
}

type review91ReplacementInspectRunner struct {
	data string
}

func (r review91ReplacementInspectRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		return []byte(r.data), nil, nil
	}
	return nil, nil, nil
}

func TestReview91LazyCacheRejectsReplacementNameLookup(t *testing.T) {
	runner := review91ReplacementInspectRunner{
		data: review91OwnedDockerInspect("replacement-uid", "replacement-generation", false, true),
	}
	ctr := &Container{
		id:       "myctr",
		runner:   runner,
		eng:      dockerEngine{},
		creation: "original-generation",
		exposed:  []portSpec{{port: 6379, proto: "tcp"}},
	}

	info, err := ctr.cachedInfo(context.Background())
	if !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("info = %+v, error = %v, want ErrGenerationReplaced", info, err)
	}
	ctr.mu.Lock()
	defer ctr.mu.Unlock()
	if ctr.uid != "" || ctr.info != nil {
		t.Fatalf("cached identity = %q, info = %+v; replacement data was pinned", ctr.uid, ctr.info)
	}
}

func TestReview91CacheKeepsIdentityAndEndpointImmutable(t *testing.T) {
	original := &engineInfo{
		state:  StateRunning,
		labels: map[string]string{managedLabel: "true", sessionLabel: sessionID(), creationLabel: "generation-a"},
		uid:    "uid-a",
		bound:  []boundPort{{containerPort: 6379, proto: "tcp", hostPort: 49153}},
	}
	replacement := &engineInfo{
		state:  StateRunning,
		labels: map[string]string{managedLabel: "true", sessionLabel: sessionID(), creationLabel: "generation-a"},
		uid:    "uid-b",
		bound:  []boundPort{{containerPort: 6379, proto: "tcp", hostPort: 49154}},
	}
	ctr := &Container{
		eng:      dockerEngine{},
		creation: "generation-a",
		uid:      "uid-a",
		exposed:  []portSpec{{port: 6379, proto: "tcp"}},
	}
	if err := ctr.cacheInfo(original); err != nil {
		t.Fatalf("cache original: %v", err)
	}
	if err := ctr.cacheInfo(replacement); !errors.Is(err, ErrGenerationReplaced) {
		t.Fatalf("cache replacement error = %v, want ErrGenerationReplaced", err)
	}

	ctr.mu.Lock()
	defer ctr.mu.Unlock()
	if ctr.uid != original.uid {
		t.Fatalf("uid = %q, want immutable %q", ctr.uid, original.uid)
	}
	if ctr.info != original {
		t.Fatalf("cached endpoint = %+v, want original %+v", ctr.info, original)
	}
}
