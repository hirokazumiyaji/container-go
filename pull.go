package container

import (
	"context"
	"fmt"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// PullPolicy decides when Run fetches the image.
type PullPolicy int

const (
	// PullMissing fetches the image only when it is not in the
	// backend's local store. This is the default and mirrors the
	// implicit pull Run always did before.
	PullMissing PullPolicy = iota
	// PullAlways fetches the image on every Run, including a
	// WithReuse attach before the shared container is returned.
	PullAlways
	// PullNever never fetches; Run fails before starting when the
	// image is absent.
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
			return fmt.Errorf("invalid pull policy %d", policy)
		}
		c.pullPolicy = policy
		return nil
	}
}

// Pull fetches the image into the backend's local store. Concurrent
// callers in the same process share one pull of the same image; a
// caller whose context is cancelled stops waiting without affecting
// the others.
func Pull(ctx context.Context, image string) error {
	if !imageRE.MatchString(image) {
		return fmt.Errorf("invalid image reference %q", image)
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
			return pullImage(execCtx, c.runner, c.eng, image, platform)
		})
	default:
		return doErr(ctx, &imageFlights, flightKey(c.eng, image, flightMissing, platform), func() error {
			execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
			defer cancel()
			exists, err := imageExists(execCtx, c.runner, c.eng, image, platform)
			if err != nil || exists {
				return err
			}
			return pullImage(execCtx, c.runner, c.eng, image, platform)
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
