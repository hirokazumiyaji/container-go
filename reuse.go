package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// reuseFlights collapses concurrent WithReuse Runs that share a name
// into one get-or-create execution.
var reuseFlights reuseFlightGroup

type reuseFlightGroup struct {
	mu       sync.Mutex
	inflight map[string]*reuseFlight
}

type reuseFlight struct {
	done chan struct{}
	ctr  *Container
	err  error
}

func (g *reuseFlightGroup) do(ctx context.Context, key string, fn func() (*Container, error)) (*Container, error) {
	g.mu.Lock()
	if g.inflight == nil {
		g.inflight = map[string]*reuseFlight{}
	}
	if f, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		return g.wait(ctx, f)
	}
	f := &reuseFlight{done: make(chan struct{})}
	g.inflight[key] = f
	g.mu.Unlock()

	go func() {
		f.ctr, f.err = fn()
		close(f.done)

		g.mu.Lock()
		delete(g.inflight, key)
		g.mu.Unlock()
	}()

	return g.wait(ctx, f)
}

func (g *reuseFlightGroup) wait(ctx context.Context, f *reuseFlight) (*Container, error) {
	select {
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		return f.ctr.sharedHandle(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Container) sharedHandle() *Container {
	return &Container{
		id:        c.id,
		runner:    c.runner,
		eng:       c.eng,
		exposed:   c.exposed,
		published: c.published,
		reused:    true,
	}
}

func reuseRun(ctx context.Context, image string, cfg *config) (*Container, error) {
	key := cfg.eng.name() + "\x00" + cfg.name
	return reuseFlights.do(ctx, key, func() (*Container, error) {
		return reuseGetOrCreate(ctx, image, cfg)
	})
}

func reuseGetOrCreate(ctx context.Context, image string, cfg *config) (*Container, error) {
	deadline := time.Now().Add(reuseAttachTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	recreated := false

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("reuse %s: timed out waiting for a usable container", cfg.name)
		}

		info, err := inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			if !isNotFound(err) {
				return nil, err
			}
			ctr, createErr := reuseCreate(ctx, image, cfg)
			if createErr == nil {
				return ctr, nil
			}
			if cfg.eng.nameConflict(createErr) {
				time.Sleep(reusePollInterval)
				continue
			}
			return nil, createErr
		}

		switch info.state {
		case StateCreated, StateStopping, StateUnknown:
			time.Sleep(reusePollInterval)
			continue
		case StateStopped:
			if recreated {
				return nil, fmt.Errorf("reuse %s: container stayed stopped after recreate", cfg.name)
			}
			if err := deleteNamed(ctx, cfg, cfg.name); err != nil {
				return nil, err
			}
			recreated = true
			continue
		case StateRunning:
			if err := checkReuseCompat(info, image, cfg); err != nil {
				return nil, err
			}
			ctr := &Container{
				id:        cfg.name,
				runner:    cfg.runner,
				eng:       cfg.eng,
				exposed:   cfg.exposed,
				published: cfg.published,
				reused:    true,
				info:      info,
			}
			if err := reuseWait(ctx, cfg, ctr); err != nil {
				return nil, err
			}
			return ctr, nil
		default:
			time.Sleep(reusePollInterval)
		}
	}
}

func reuseCreate(ctx context.Context, image string, cfg *config) (*Container, error) {
	var envFile string
	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFile(cfg.env)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		envFile = path
	}

	runCtx, cancel := withDefaultTimeout(ctx, runTimeout)
	defer cancel()
	if err := cfg.ensureImage(runCtx, image); err != nil {
		return nil, err
	}
	if _, _, err := cfg.runner.Run(runCtx, cfg.eng.runArgs(cfg, image, envFile)...); err != nil {
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		if cfg.eng.nameConflict(err) || cfg.eng.nameConflict(classified) {
			return nil, err
		}
		return nil, classified
	}

	ctr := &Container{
		id:        cfg.name,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		reused:    true,
	}
	if _, err := ctr.cachedInfo(ctx); err != nil {
		_ = ctr.Terminate(context.WithoutCancel(ctx))
		return nil, err
	}
	for _, f := range cfg.files {
		if err := ctr.CopyToContainer(ctx, f.HostPath, f.ContainerPath); err != nil {
			_ = ctr.Terminate(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	if err := reuseWait(ctx, cfg, ctr); err != nil {
		return nil, err
	}
	return ctr, nil
}

func reuseWait(ctx context.Context, cfg *config, ctr *Container) error {
	if cfg.waitStrategy == nil {
		return nil
	}
	if err := cfg.waitStrategy.WaitUntilReady(ctx, waitTarget{c: ctr}); err != nil {
		tail := ctr.logTail(context.WithoutCancel(ctx))
		if tail != "" {
			return fmt.Errorf("reuse %s failed to become ready: %w\ncontainer logs:\n%s", ctr.id, err, tail)
		}
		return fmt.Errorf("reuse %s failed to become ready: %w", ctr.id, err)
	}
	return nil
}

func inspectNamed(ctx context.Context, cfg *config, id string) (*engineInfo, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := cfg.runner.Run(qCtx, cfg.eng.inspectArgs(id)...)
	if err != nil {
		return nil, cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
	}
	return cfg.eng.parseInspect(stdout, id)
}

func deleteNamed(ctx context.Context, cfg *config, id string) error {
	dCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	_, _, err := cfg.runner.Run(dCtx, cfg.eng.deleteArgs(id)...)
	if err == nil || isNotFound(err) {
		return nil
	}
	return cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
}

func checkReuseCompat(info *engineInfo, image string, cfg *config) error {
	if info.labels[reuseLabel] != "true" {
		return fmt.Errorf("reuse %s: existing container was not created with WithReuse", cfg.name)
	}
	if !imagesCompatible(image, info.image) {
		return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, image, info.image)
	}
	if cfg.eng.directIP() {
		return nil
	}
	for _, spec := range cfg.exposed {
		if !hasBoundPort(info.bound, spec.port, spec.proto) {
			return fmt.Errorf("reuse %s: exposed port %s missing on existing container", cfg.name, spec)
		}
	}
	for _, p := range cfg.published {
		if !hasPublishedBinding(info.bound, p) {
			return fmt.Errorf("reuse %s: published port %s missing on existing container", cfg.name, p.raw)
		}
	}
	return nil
}

func hasBoundPort(bound []boundPort, port int, proto string) bool {
	for _, b := range bound {
		if b.containerPort == port && b.proto == proto && b.hostPort > 0 {
			return true
		}
	}
	return false
}

func hasPublishedBinding(bound []boundPort, p publishSpec) bool {
	for _, b := range bound {
		if b.containerPort != p.containerPort || b.proto != p.proto {
			continue
		}
		if p.hostPort != 0 && b.hostPort != p.hostPort {
			continue
		}
		if p.hostAddr != "" && b.hostAddr != "" && b.hostAddr != p.hostAddr {
			continue
		}
		return true
	}
	return false
}

// imagesCompatible reports whether a requested image reference matches
// what inspect reported. Docker may expand short names to
// docker.io/library/...; digests are accepted when they share a name.
func imagesCompatible(requested, actual string) bool {
	if requested == "" || actual == "" {
		return requested == actual
	}
	if requested == actual {
		return true
	}
	req := stripImageDigest(requested)
	act := stripImageDigest(actual)
	if req == act {
		return true
	}
	if strings.HasSuffix(act, "/"+req) || strings.HasSuffix(act, ":"+req) {
		return true
	}
	return imageNameTag(req) == imageNameTag(act)
}

func stripImageDigest(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i]
	}
	return ref
}

func imageNameTag(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	return ref
}

// PruneReuseGroup force-removes every container tagged with the given
// WithReuseGroup value, running or stopped. Use it as a CI teardown
// step; ordinary Prune still only removes stopped managed containers.
func PruneReuseGroup(ctx context.Context, group string) ([]string, error) {
	if group == "" {
		return nil, fmt.Errorf("reuse group must not be empty")
	}
	eng, err := detectEngine()
	if err != nil {
		return nil, err
	}
	return pruneReuseGroupWith(ctx, &cli.ExecRunner{Binary: eng.binary()}, eng, group)
}

func pruneReuseGroupWith(ctx context.Context, r cli.Runner, eng engine, group string) ([]string, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := r.Run(qCtx, eng.listReuseGroupArgs(group)...)
	if err != nil {
		return nil, cli.Classify(ctx, r, err, eng.probe())
	}
	ids, err := eng.parseReuseGroupIDs(stdout, group)
	if err != nil {
		return nil, err
	}

	var removed []string
	var errs []error
	for _, id := range ids {
		dCtx, dCancel := withDefaultTimeout(ctx, queryTimeout)
		_, _, err := r.Run(dCtx, eng.deleteArgs(id)...)
		dCancel()
		if err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("prune reuse group %s: %s: %w", group, id, err))
			continue
		}
		removed = append(removed, id)
	}
	if len(errs) > 0 {
		return removed, errors.Join(errs...)
	}
	return removed, nil
}
