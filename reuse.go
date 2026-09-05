package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// reuseFlights collapses concurrent WithReuse get-or-create calls that
// share a name into one ensure operation. Each caller still applies its
// own compatibility check and wait strategy afterward.
var reuseFlights flightGroup[*Container]

func reuseRun(ctx context.Context, image string, cfg *config) (*Container, error) {
	key := cfg.eng.name() + "\x00" + cfg.name
	base, err := reuseFlights.do(ctx, key, func() (*Container, error) {
		// Shared ensure must not die with the first caller's cancel;
		// waiters keep waiting on their own contexts.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reuseAttachTimeout)
		defer cancel()
		return reuseEnsureContainer(flightCtx, image, cfg)
	})
	if err != nil {
		return nil, err
	}

	info := base.info
	if info == nil {
		info, err = inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			return nil, err
		}
	}
	if err := checkReuseCompat(info, image, cfg); err != nil {
		return nil, err
	}

	ctr := &Container{
		id:        base.id,
		runner:    base.runner,
		eng:       base.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		reused:    true,
		info:      info,
	}
	if err := reuseWait(ctx, cfg, ctr); err != nil {
		return nil, err
	}
	return ctr, nil
}

// reuseEnsureContainer creates or attaches to the named container
// without per-caller wait or port compatibility checks. Those run in
// reuseRun so every concurrent caller applies its own configuration.
func reuseEnsureContainer(ctx context.Context, image string, cfg *config) (*Container, error) {
	recreated := false

	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("reuse %s: timed out waiting for a usable container", cfg.name)
			}
			return nil, err
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
			// Only recycle containers this library created for reuse
			// with a compatible image; never delete foreign leftovers.
			if err := checkReuseOwned(info, image, cfg); err != nil {
				return nil, err
			}
			if err := deleteNamed(ctx, cfg, cfg.name); err != nil {
				return nil, err
			}
			recreated = true
			continue
		case StateRunning:
			return &Container{
				id:        cfg.name,
				runner:    cfg.runner,
				eng:       cfg.eng,
				exposed:   cfg.exposed,
				published: cfg.published,
				reused:    true,
				info:      info,
			}, nil
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
	return namedContainer(cfg, id).inspectFresh(ctx)
}

func deleteNamed(ctx context.Context, cfg *config, id string) error {
	return namedContainer(cfg, id).Terminate(ctx)
}

func namedContainer(cfg *config, id string) *Container {
	return &Container{
		id:        id,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
	}
}

// checkReuseOwned reports whether a stopped container may be deleted
// and recreated for this reuse request.
func checkReuseOwned(info *engineInfo, image string, cfg *config) error {
	if info.labels[reuseLabel] != "true" {
		return fmt.Errorf("reuse %s: existing container was not created with WithReuse", cfg.name)
	}
	if !imagesCompatible(image, info.image) {
		return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, image, info.image)
	}
	return nil
}

func checkReuseCompat(info *engineInfo, image string, cfg *config) error {
	if err := checkReuseOwned(info, image, cfg); err != nil {
		return err
	}
	// Auto-published exposed ports only appear as host bindings on
	// published-port backends. Explicit WithPublishedPort always needs
	// validation, including on direct-IP engines.
	if !cfg.eng.directIP() {
		for _, spec := range cfg.exposed {
			if !hasBoundPort(info.bound, spec.port, spec.proto) {
				return fmt.Errorf("reuse %s: exposed port %s missing on existing container", cfg.name, spec)
			}
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
// docker.io/library/.... When the request pins a digest, that digest
// must appear on the existing image.
func imagesCompatible(requested, actual string) bool {
	if requested == "" || actual == "" {
		return requested == actual
	}
	if requested == actual {
		return true
	}
	if reqDigest := imageDigest(requested); reqDigest != "" {
		return reqDigest == imageDigest(actual)
	}
	req := stripImageDigest(requested)
	act := stripImageDigest(actual)
	if req == act {
		return true
	}
	return strings.HasSuffix(act, "/"+req) || strings.HasSuffix(req, "/"+act)
}

func imageDigest(ref string) string {
	i := strings.Index(ref, "@")
	if i < 0 {
		return ""
	}
	return ref[i+1:]
}

func stripImageDigest(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i]
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
	return pruneListed(ctx, r, eng, eng.listReuseGroupArgs(group), func(data []byte) ([]string, error) {
		return eng.parseReuseGroupIDs(data, group)
	}, "prune reuse group "+group)
}
