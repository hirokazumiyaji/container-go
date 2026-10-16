package container

import "context"

// applePullNeverBelowPreflight lets tests exercise the image-identity layer
// that sits underneath the Apple PullNever rejection. Public Run rejects the
// policy in checkConfig (see TestApplePullNeverIsUnsupported),
// so these tests would otherwise never reach that layer.
type applePullNeverBelowPreflight struct{ appleEngine }

func (e applePullNeverBelowPreflight) checkConfig(ctx context.Context, cfg *config) error {
	policy := cfg.pullPolicy
	if policy == PullNever {
		cfg.pullPolicy = PullMissing
	}
	err := e.appleEngine.checkConfig(ctx, cfg)
	cfg.pullPolicy = policy
	return err
}
