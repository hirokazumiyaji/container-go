package domain

// Label constants applied by container-go to track managed containers.
const (
	LabelManaged      = "dev.containergo.managed"
	LabelSession      = "dev.containergo.session"
	LabelCreation     = "dev.containergo.creation"
	LabelReuse        = "dev.containergo.reuse"
	LabelReuseGroup   = "dev.containergo.reuse-group"
	LabelReuseImage   = "dev.containergo.reuse-image"
	LabelReuseDigest  = "dev.containergo.reuse-digest"
	LabelReuseExposed = "dev.containergo.reuse-exposed"
	LabelReuseEnv     = "dev.containergo.reuse-env"
)

// LabelConfig holds the fields needed to construct standard container-go labels.
type LabelConfig struct {
	Session      string
	Creation     string
	Reuse        bool
	ReuseGroup   string
	ReuseImage   string
	ReuseDigest  string
	ReuseExposed string
	ReuseEnv     string
	Custom       map[string]string
}

// BuildLabels merges custom labels with container-go tracking labels.
func BuildLabels(cfg LabelConfig) map[string]string {
	labels := make(map[string]string, len(cfg.Custom)+6)
	for k, v := range cfg.Custom {
		labels[k] = v
	}
	labels[LabelManaged] = "true"
	if cfg.Session != "" {
		labels[LabelSession] = cfg.Session
	}
	if cfg.Creation != "" {
		labels[LabelCreation] = cfg.Creation
	}
	if cfg.Reuse {
		labels[LabelReuse] = "true"
		if cfg.ReuseGroup != "" {
			labels[LabelReuseGroup] = cfg.ReuseGroup
		}
		if cfg.ReuseImage != "" {
			labels[LabelReuseImage] = cfg.ReuseImage
		}
		if cfg.ReuseDigest != "" {
			labels[LabelReuseDigest] = cfg.ReuseDigest
		}
		if cfg.ReuseExposed != "" {
			labels[LabelReuseExposed] = cfg.ReuseExposed
		}
		if cfg.ReuseEnv != "" {
			labels[LabelReuseEnv] = cfg.ReuseEnv
		}
	}
	return labels
}
