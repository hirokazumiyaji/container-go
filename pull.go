package container

import (
	"context"
	"fmt"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// PullPolicy decides when Run fetches the image.
type PullPolicy int

const (
	// PullMissing fetches the image only when it is not in the
	// backend's local store. This is the default and mirrors the
	// implicit pull Run always did before.
	PullMissing PullPolicy = iota
	// PullAlways fetches the image on every Run.
	PullAlways
	// PullNever never fetches; Run fails before starting when the
	// image is absent or when a pinned identity is not locally
	// addressable by the backend.
	PullNever
)

// Flight operation kinds keep mandatory pulls from joining an
// inspect-only PullMissing flight for the same image.
const (
	flightPull    = "pull"
	flightMissing = "missing"
)

// WithPullPolicy sets when Run fetches the image. The default is
// PullMissing.
func WithPullPolicy(policy PullPolicy) Option {
	return func(c *config) error {
		if policy < PullMissing || policy > PullNever {
			return fmt.Errorf("invalid pull policy %d", policy)
		}
		c.pullPolicy = policy
		return nil
	}
}

// WithAllowMutableImageTag permits Run to execute the caller's original
// mutable tag when the backend's image-inspect response contains no
// digest or immutable local ID, or when an Apple ID-derived digest
// cannot be addressed locally. This is an explicit compatibility
// escape hatch: it does not prevent a tag from being replaced after
// inspection, so it is not an identity guarantee. It never downgrades a
// caller-supplied digest. Without this option Run fails closed with
// ErrImageIdentityUnavailable.
func WithAllowMutableImageTag() Option {
	return func(c *config) error {
		c.allowMutableImageTag = true
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

// imageResolutionFlights shares the inspect/pull/identity transaction
// for PullMissing callers and the post-pull identity resolution for
// PullAlways callers. The pull itself remains in imageFlights so the
// public Pull API keeps sharing it with PullAlways.
var imageResolutionFlights flightGroup[imageIdentity]

// imageIdentityParser is an optional backend capability. Keeping it
// separate from engine lets older/injected engines retain the existence
// check while the default Run policy fails closed without an identity.
type imageIdentityParser interface {
	parseImageIdentity(data []byte, image, platform string) (imageIdentity, bool)
}

type localImageIdentityChecker interface {
	imageIdentityNeedsLocalCheck() bool
}

func imageIdentityNeedsLocalCheck(eng engine) bool {
	checker, ok := eng.(localImageIdentityChecker)
	return ok && checker.imageIdentityNeedsLocalCheck()
}

// imageIdentityNeedsLocalAddressCheck identifies the pinned references
// that must be proven addressable before they are passed to a backend
// without a runtime pull-never switch. PullNever always checks. Apple's
// ID-derived digest is also checked for other policies because it names
// local content rather than a registry manifest that can be fetched.
func imageIdentityNeedsLocalAddressCheck(eng engine, identity imageIdentity, policy PullPolicy) bool {
	if !imageIdentityNeedsLocalCheck(eng) || !identity.pinned {
		return false
	}
	return policy == PullNever || (eng.name() == "apple" && identity.id != "")
}

// ensureImage brings the image into the backend's local store according
// to the pull policy. It is retained as the error-only helper used by
// existing internal callers; Run uses ensureImageRef so the immutable
// identity is carried into the run command.
func (c *config) ensureImage(ctx context.Context, image string) error {
	_, err := c.ensureImageRef(ctx, image)
	return err
}

func (c *config) ensureImageRef(ctx context.Context, image string) (imageIdentity, error) {
	platform := c.platform
	switch c.pullPolicy {
	case PullNever:
		return c.resolveImage(ctx, image, platform)
	case PullAlways:
		if err := doErr(ctx, &imageFlights, flightKey(c.eng, image, flightPull, platform), func() error {
			execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
			defer cancel()
			return pullImage(execCtx, c.runner, c.eng, image, platform)
		}); err != nil {
			return imageIdentity{}, err
		}
		resolveKey := flightKey(c.eng, image, flightPull, platform) + "\x00resolve"
		if c.allowMutableImageTag {
			resolveKey += "\x00allow-mutable-tag"
		}
		return imageResolutionFlights.do(ctx, resolveKey, func() (imageIdentity, error) {
			execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
			defer cancel()
			return c.resolveImage(execCtx, image, platform)
		})
	default:
		key := flightKey(c.eng, image, flightMissing, platform)
		if c.allowMutableImageTag {
			// A fallback-enabled caller must not share a result with a
			// default fail-closed caller (or vice versa).
			key += "\x00allow-mutable-tag"
		}
		return imageResolutionFlights.do(ctx, key, func() (imageIdentity, error) {
			execCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
			defer cancel()
			identity, exists, err := inspectImage(execCtx, c.runner, c.eng, image, platform)
			if err != nil {
				return imageIdentity{}, err
			}
			if exists {
				return c.resolveInspectedImage(execCtx, image, platform, identity)
			}
			if err := pullImage(execCtx, c.runner, c.eng, image, platform); err != nil {
				return imageIdentity{}, err
			}
			return c.resolveImage(execCtx, image, platform)
		})
	}
}

// resolveImage requires a successful inspect and turns the backend's
// reported identity into the argument that Run must use. A caller may
// explicitly opt into the old mutable-tag behavior, but the default is
// fail closed when the backend cannot identify the inspected image.
func (c *config) resolveImage(ctx context.Context, image, platform string) (imageIdentity, error) {
	identity, exists, err := inspectImage(ctx, c.runner, c.eng, image, platform)
	if err != nil {
		return imageIdentity{}, err
	}
	if !exists {
		if platform != "" {
			return imageIdentity{}, fmt.Errorf("%w: %s for platform %s", ErrImageNotFound, image, platform)
		}
		return imageIdentity{}, fmt.Errorf("%w: %s", ErrImageNotFound, image)
	}
	return c.resolveInspectedImage(ctx, image, platform, identity)
}

func (c *config) resolveInspectedImage(ctx context.Context, image, platform string, identity imageIdentity) (imageIdentity, error) {
	pinned, err := c.pinImage(image, identity)
	if err != nil {
		return imageIdentity{}, err
	}
	if !imageIdentityNeedsLocalAddressCheck(c.eng, pinned, c.pullPolicy) || pinned.reference == image {
		return pinned, nil
	}

	// Apple Container resolves a missing reference during `container run`
	// because it has no --pull=never switch. Check the pinned reference
	// first, and explicitly pull it when the current policy permits a
	// fetch. This keeps an ID-derived local reference from silently
	// becoming a registry lookup.
	checked, pinnedExists, err := inspectImage(ctx, c.runner, c.eng, pinned.reference, platform)
	if err != nil {
		return imageIdentity{}, fmt.Errorf("%w: %w: could not inspect locally addressable image %s: %w", ErrImageIdentityUnavailable, ErrImageIdentityNotLocal, pinned.reference, err)
	}
	if !pinnedExists && c.pullPolicy != PullNever {
		if pullErr := pullImage(ctx, c.runner, c.eng, pinned.reference, platform); pullErr == nil {
			checked, pinnedExists, err = inspectImage(ctx, c.runner, c.eng, pinned.reference, platform)
			if err != nil {
				return imageIdentity{}, fmt.Errorf("%w: %w: could not verify locally addressable image %s: %w", ErrImageIdentityUnavailable, ErrImageIdentityNotLocal, pinned.reference, err)
			}
		} else if c.canUseMutableFallback(image) {
			return imageIdentity{reference: image}, nil
		} else {
			return imageIdentity{}, fmt.Errorf("%w: %w: %s could not be made locally addressable: %w", ErrImageIdentityUnavailable, ErrImageIdentityNotLocal, pinned.reference, pullErr)
		}
	}
	if !pinnedExists {
		if c.canUseMutableFallback(image) {
			return imageIdentity{reference: image}, nil
		}
		return imageIdentity{}, fmt.Errorf("%w: %w: %s cannot run %s without fetching it", ErrImageIdentityUnavailable, ErrImageIdentityNotLocal, c.eng.name(), pinned.reference)
	}
	if checked.mismatch {
		return imageIdentity{}, fmt.Errorf("%w: pinned reference %s resolves to another image", ErrImageIdentityMismatch, pinned.reference)
	}
	if !checked.pinned {
		if c.canUseMutableFallback(image) {
			return imageIdentity{reference: image}, nil
		}
		return imageIdentity{}, fmt.Errorf("%w: pinned reference %s did not report an immutable identity", ErrImageIdentityUnavailable, pinned.reference)
	}
	if !imageIdentitiesCompatible(pinned, checked) {
		return imageIdentity{}, fmt.Errorf("%w: pinned reference %s changed before run", ErrImageIdentityMismatch, pinned.reference)
	}
	return pinned, nil
}

func (c *config) pinImage(image string, identity imageIdentity) (imageIdentity, error) {
	if identity.mismatch {
		return imageIdentity{}, fmt.Errorf("%w: %s", ErrImageIdentityMismatch, image)
	}
	if identity.pinned {
		if requestedDigest := imageDigest(image); validImageDigest(requestedDigest) &&
			identity.digest != "" && requestedDigest != identity.digest {
			return imageIdentity{}, fmt.Errorf("%w: %q changed during resolution: requested %s, inspected %s", ErrImageIdentityMismatch, image, requestedDigest, identity.digest)
		}
		return identity, nil
	}
	// A successful identity-less Apple inspect cannot establish that a
	// caller-pinned digest is the local image, and PullNever must not
	// let run perform an implicit fetch. Mutable tags remain available
	// through the explicit compatibility option below.
	immutableRequest := isImageID(image) || strings.Contains(image, "@")
	if c.pullPolicy == PullNever && c.eng.name() == "apple" && immutableRequest {
		return imageIdentity{}, fmt.Errorf("%w: %s (Apple image inspect did not confirm the pinned identity)", ErrImageIdentityUnavailable, image)
	}
	// A caller-supplied digest or local image ID is already immutable,
	// even if an older backend cannot repeat that identity in its
	// inspect JSON.
	if isImageID(image) {
		return imageIdentity{reference: image, digest: image, id: image, pinned: true}, nil
	}
	if requestedDigest := imageDigest(image); validImageDigest(requestedDigest) {
		return imageIdentity{reference: image, digest: requestedDigest, pinned: true}, nil
	}
	if c.canUseMutableFallback(image) {
		return imageIdentity{reference: image}, nil
	}
	return imageIdentity{}, fmt.Errorf("%w: %s (%s image inspect did not report a usable digest or immutable ID)", ErrImageIdentityUnavailable, image, c.eng.name())
}

func (c *config) canUseMutableFallback(image string) bool {
	return c.allowMutableImageTag && !isImageID(image) && !strings.Contains(image, "@")
}

// inspectImage returns both existence and the identity reported by the
// backend. A non-empty inspect array can be present without an identity;
// callers must not interpret that as safe to execute a mutable tag.
func inspectImage(ctx context.Context, r cli.Runner, eng engine, image, platform string) (imageIdentity, bool, error) {
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	stdout, _, err := r.Run(qCtx, eng.imageInspectArgs(image, platform)...)
	if err != nil {
		if eng.imageMissing(err) {
			return imageIdentity{}, false, nil
		}
		return imageIdentity{}, false, cli.Classify(ctx, r, err, eng.probe())
	}
	if parser, ok := eng.(imageIdentityParser); ok {
		identity, exists := parser.parseImageIdentity(stdout, image, platform)
		return identity, exists, nil
	}
	return imageIdentity{}, eng.parseImageExists(stdout, platform), nil
}

// imageExists reports whether the image (and requested platform
// variant, when set) is in the backend's store.
func imageExists(ctx context.Context, r cli.Runner, eng engine, image, platform string) (bool, error) {
	_, exists, err := inspectImage(ctx, r, eng, image, platform)
	return exists, err
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
