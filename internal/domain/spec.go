package domain

// Spec represents an immutable container execution specification.
type Spec struct {
	Image      string
	Name       string
	Entrypoint string
	Cmd        []string
	Env        map[string]string
	Workdir    string
	User       string
	Network    string
	Platform   string
	CPUs       int
	Memory     string
	Labels     map[string]string
	Exposed    []PortSpec
	Published  []PublishSpec
	Reuse      bool
	ReuseGroup string
	Creation   string
}

// Clone returns a deep copy of the Spec.
func (s Spec) Clone() Spec {
	cp := s
	if s.Cmd != nil {
		cp.Cmd = append([]string(nil), s.Cmd...)
	}
	if s.Env != nil {
		cp.Env = make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			cp.Env[k] = v
		}
	}
	if s.Labels != nil {
		cp.Labels = make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			cp.Labels[k] = v
		}
	}
	if s.Exposed != nil {
		cp.Exposed = append([]PortSpec(nil), s.Exposed...)
	}
	if s.Published != nil {
		cp.Published = append([]PublishSpec(nil), s.Published...)
	}
	return cp
}
