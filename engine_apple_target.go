package container

// target returns nil: Apple Container has one local service and no externally
// addressable daemon target.
func (appleEngine) target() engineTarget { return nil }

// defaultRunPlatform reports Apple Container's host-architecture Linux
// platform, which the run command applies when the caller selects none.
func (appleEngine) defaultRunPlatform() string { return appleHostPlatform() }
