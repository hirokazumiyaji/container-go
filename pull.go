package container

import (
	"context"
	"fmt"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// PullPolicy decides when Run fetches the image. PullNever is a
// backend-specific best-effort precheck on this checkout: Docker passes
// --pull=never, while Apple Container has no equivalent run-time switch
// and may resolve the image during its own run path. #112 tracks strict
// Apple capability handling.
type PullPolicy int

const (
	// PullMissing fetches the image only when it is not in the
	// backend's local store. This is the default and mirrors the
	// implicit pull Run always did before.
	PullMissing PullPolicy = iota
	// PullAlways fetches the image on every Run, including a
	// WithReuse attach before the shared container is returned.
	PullAlways
	// PullNever performs the current backend precheck and returns
	// ErrImageNotFound when the image is absent. Docker also passes
	// --pull=never, but Apple Container has no equivalent flag; its
	// best-effort precheck is not a no-fetch guarantee (#112).
	PullNever
)

// Flight operation kinds keep mandatory pulls from joining an
// inspect-only PullMissing flight for the same image.
const (
	flightPull    = "pull"
	flightMissing = "missing"
)

// WithPullPolicy sets when Run fetches the image. The default is
// PullMissing. PullAlways is honored for every WithReuse caller,
// including attach callers.
func WithPullPolicy(policy PullPolicy) Option {
	return func(c *config) error {
		if policy < PullMissing || policy > PullNever {
			return validationErrorf("WithPullPolicy", policy, "invalid pull policy %d", policy)
		}
		c.pullPolicy = policy
		return nil
	}
}

// WithImagePresenceCache reuses the answer to "is this image already in
// the local store?" for up to ttl, so a second Run of the same image does
// not spawn an `image inspect` against the daemon.
//
// Off by default: Run's default PullMissing policy inspects on every
// call, and caching that answer means an image removed out of band (by
// another tool, or by a CI cache prune) would not be re-pulled until the
// entry expired. Enable it when the daemon's store is known to be stable
// for the duration of the test run and the round trip is worth avoiding.
//
// Only "present" answers are cached. A missing image is pulled, and the
// successful pull is recorded, so the next Run of the same image skips
// the inspect too.
//
// The cache is owned by the returned Option, not process-global: call
// WithImagePresenceCache once and reuse that Option across Runs so
// entries persist. Each Run still builds a fresh config, but applying the
// same Option attaches the same cache. A fresh call to
// WithImagePresenceCache creates a distinct empty cache, so callers that
// do not share Options cannot observe each other's entries. Concurrent
// Runs of the same image in the same process still collapse onto one
// flight; this option removes the inspect cost across sequential Runs.
//
// A ttl of zero or less disables the cache, which is the same as not
// passing this option. Run without this option always inspects, so the
// cache is a cost decision the caller makes explicitly.
func WithImagePresenceCache(ttl time.Duration) Option {
	if ttl < 0 {
		return func(*config) error {
			return fmt.Errorf("invalid image presence cache ttl %v: must not be negative", ttl)
		}
	}
	cache := newImageCache(ttl)
	return func(c *config) error {
		c.imageCache = cache
		return nil
	}
}

// Pull fetches the image into the backend's local store. Concurrent
// callers in the same process share one pull of the same image; a
// caller whose context is cancelled stops waiting without affecting
// the others.
func Pull(ctx context.Context, image string) error {
	if err := validateImageReference(image); err != nil {
		return err
	}
	eng, err := detectEngine()
	if err != nil {
		return err
	}
	return pullWith(ctx, &cli.ExecRunner{Binary: eng.binary()}, eng, image)
}

// pullWith is the fake-runner-driven core of Pull: the flight shares one
// backend pull of the image, and failures go through Classify.
func pullWith(ctx context.Context, r cli.Runner, eng engine, image string) error {
	return doErr(ctx, &imageFlights, flightKey(eng, image, flightPull, ""), func() error {
		execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
		defer cancel()
		return pullImage(execCtx, r, eng, image, "")
	})
}

// flightKey identifies the pull being aggregated: one backend, one
// image, one operation kind, one platform. Same image with different
// platforms must not share a flight.
func flightKey(eng engine, image, op, platform string) string {
	return eng.name() + "\x00" + image + "\x00" + op + "\x00" + platform
}

// imageFlights aggregates concurrent image fetches across the process.
// Entries are always removed on completion, so a failed pull is
// retried on the next call.
var imageFlights flightGroup[struct{}]

// ensureImage brings the image into the backend's local store according
// to the pull policy. It runs once per concurrent set of Runs sharing
// the image, operation, and platform: the first caller inspects and/or
// pulls, the rest wait.
func (c *config) ensureImage(ctx context.Context, image string) error {
	platform := c.platform
	if c.pullPolicy == PullMissing && c.imageCache.seen(c.eng, image, platform) {
		return nil
	}
	switch c.pullPolicy {
	case PullNever:
		exists, err := imageExists(ctx, c.runner, c.eng, image, platform)
		if err != nil {
			return err
		}
		if !exists {
			if platform != "" {
				return fmt.Errorf("%w: %s for platform %s", ErrImageNotFound, image, platform)
			}
			return fmt.Errorf("%w: %s", ErrImageNotFound, image)
		}
		return nil
	case PullAlways:
		return doErr(ctx, &imageFlights, flightKey(c.eng, image, flightPull, platform), func() error {
			execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
			defer cancel()
			// A successful PullAlways fetch makes the image present, so
			// drop any entry that disagrees before recording the new one.
			c.imageCache.forget(c.eng, image, platform)
			if err := pullImage(execCtx, c.runner, c.eng, image, platform); err != nil {
				return err
			}
			c.imageCache.remember(c.eng, image, platform)
			return nil
		})
	default:
		return doErr(ctx, &imageFlights, flightKey(c.eng, image, flightMissing, platform), func() error {
			execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
			defer cancel()
			exists, err := imageExists(execCtx, c.runner, c.eng, image, platform)
			if err != nil {
				return err
			}
			if exists {
				// Record the presence so a later Run of the same image
				// can skip the inspect entirely.
				c.imageCache.remember(c.eng, image, platform)
				return nil
			}
			if err := pullImage(execCtx, c.runner, c.eng, image, platform); err != nil {
				return err
			}
			// The pull just made the image present; remember that rather
			// than making the next Run inspect to find out.
			c.imageCache.remember(c.eng, image, platform)
			return nil
		})
	}
}

// imageExists reports whether the image (and requested platform
// variant, when set) is in the backend's store.
func imageExists(ctx context.Context, r cli.Runner, eng engine, image, platform string) (bool, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := r.Run(qCtx, eng.imageInspectArgs(image, platform)...)
	if err != nil {
		if eng.imageMissing(err) {
			return false, nil
		}
		return false, cli.Classify(ctx, r, err, eng.probe())
	}
	return eng.parseImageExists(stdout, platform), nil
}

// pullImage fetches the image (and requested platform variant, when
// set) through the backend CLI.
func pullImage(ctx context.Context, r cli.Runner, eng engine, image, platform string) error {
	_, _, err := r.Run(ctx, eng.pullImageArgs(image, platform)...)
	if err != nil {
		return cli.Classify(ctx, r, err, eng.probe())
	}
	return nil
}
