package container

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type unboundDockerReplacementRunner struct {
	inspectJSON string
}

func (r *unboundDockerReplacementRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	if args[0] == "inspect" {
		return []byte(r.inspectJSON), nil, nil
	}
	return nil, nil, nil
}

func unboundDockerReplacementInspect(uid, name, creation string) string {
	return fmt.Sprintf(`[{"Id":%q,"Name":%q,"State":{"Status":"running"},"Config":{"Image":"redis:7-alpine","Labels":{"%s":"true","%s":"true","%s":%q}},"NetworkSettings":{"IPAddress":"172.17.0.2"}}]`,
		uid, "/"+name, managedLabel, reuseLabel, creationLabel, creation)
}

func TestReuseDoesNotBindDockerUIDFromUnverifiedInspect(t *testing.T) {
	const (
		uid          = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		creation     = "aaaaaaaaaaaaaaaa"
		containerKey = "unverified-docker-uid"
	)
	runner := &unboundDockerReplacementRunner{
		inspectJSON: unboundDockerReplacementInspect(uid, containerKey, creation),
	}
	cfg := &config{
		runner: runner,
		eng:    dockerEngine{},
		name:   containerKey,
		reuse:  true,
	}
	base := &Container{
		id:       containerKey,
		runner:   runner,
		eng:      dockerEngine{},
		reused:   true,
		creation: creation,
		// An empty UID models malformed/empty Docker run output. The
		// replacement UID below is only an inspect result, not a verified
		// handle identity.
		uid: "",
		info: &engineInfo{uid: uid, state: StateRunning, image: "redis:7-alpine", labels: map[string]string{
			managedLabel:  "true",
			reuseLabel:    "true",
			creationLabel: creation,
		}},
	}

	key := cfg.eng.name() + "\x00" + cfg.name
	started := make(chan struct{})
	joined := make(chan struct{})
	release := make(chan struct{})
	seedDone := make(chan error, 1)
	oldOnJoin := reuseFlights.onJoin
	var joinOnce sync.Once
	reuseFlights.onJoin = func(string) {
		joinOnce.Do(func() { close(joined) })
	}
	t.Cleanup(func() { reuseFlights.onJoin = oldOnJoin })
	var releaseOnce sync.Once
	releaseFlight := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFlight)
	go func() {
		_, err := reuseFlights.do(context.Background(), key, func() (*Container, error) {
			close(started)
			<-release
			return base, nil
		})
		seedDone <- err
	}()
	<-started
	go func() {
		<-joined
		releaseFlight()
	}()

	ctr, err := reuseRun(context.Background(), "redis:7-alpine", cfg)
	if err == nil {
		t.Fatalf("reuseRun returned an unverified handle: %#v", ctr)
	}
	if ctr != nil {
		t.Fatalf("reuseRun returned a handle with UID %q alongside error", ctr.uid)
	}
	if !strings.Contains(err.Error(), "immutable ID") {
		t.Fatalf("reuseRun error = %v, want immutable-ID refusal", err)
	}
	if err := <-seedDone; err != nil {
		t.Fatalf("seed reuse flight: %v", err)
	}
}

func TestReuseBaseIdentityRequiresDockerGeneration(t *testing.T) {
	base := &Container{
		uid:      "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		creation: "",
	}
	if err := validateReuseBaseIdentity(dockerEngine{}, base, "generation-required"); err == nil {
		t.Fatal("Docker base with an unbound creation generation was accepted")
	}
}

func TestUnboundDockerStateAndIPDoNotPublishReplacementUID(t *testing.T) {
	const uid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	runner := &unboundDockerReplacementRunner{
		inspectJSON: unboundDockerReplacementInspect(uid, "unbound-state", "aaaaaaaaaaaaaaaa"),
	}
	ctr := &Container{
		id:          "unbound-state",
		runner:      runner,
		eng:         dockerEngine{},
		nameInspect: true,
	}
	if state, err := ctr.State(context.Background()); err != nil || state != StateRunning {
		t.Fatalf("State = (%s, %v)", state, err)
	}
	if ip, err := ctr.ContainerIP(context.Background()); err != nil || ip != "172.17.0.2" {
		t.Fatalf("ContainerIP = (%q, %v)", ip, err)
	}
	if ctr.uid != "" {
		t.Fatalf("State/ContainerIP published replacement UID: %q", ctr.uid)
	}
	if _, err := ctr.verifiedOperationTarget(); err == nil {
		t.Fatal("unverified Docker handle became operational")
	}
}
