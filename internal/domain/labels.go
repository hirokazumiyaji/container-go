package domain

// Label constants applied by container-go to track managed containers.
const (
	LabelManaged             = "com.github.hirokazumiyaji.container-go"
	LabelSession             = "com.github.hirokazumiyaji.container-go.session"
	LabelCreation            = "com.github.hirokazumiyaji.container-go.creation"
	LabelReuse               = "com.github.hirokazumiyaji.container-go.reuse"
	LabelReuseConfig         = "com.github.hirokazumiyaji.container-go.reuse-config"
	LabelReuseGroup          = "com.github.hirokazumiyaji.container-go.reuse-group"
	LabelReuseEnvFingerprint = "com.github.hirokazumiyaji.container-go.reuse-env-fingerprint"
)

// LabelConfig holds the fields needed to construct standard container-go labels.
type LabelConfig struct {
	Session    string
	Creation   string
	Reuse      bool
	ReuseGroup string
	// ReuseConfig is the creation-fingerprint value written only when Reuse
	// is true. Leaving it empty omits the label.
	ReuseConfig string
	// ReuseEnvFingerprint is the opt-in environment digest written only
	// when Reuse is true. It never contains raw environment values.
	ReuseEnvFingerprint string
	Custom              map[string]string
}

// IsReservedLabel reports whether key is owned by container-go. Custom labels
// may not set these keys: the internal value is the only trustworthy source
// for ownership and cleanup decisions.
func IsReservedLabel(key string) bool {
	switch key {
	case LabelManaged, LabelSession, LabelCreation, LabelReuse, LabelReuseGroup, LabelReuseConfig, LabelReuseEnvFingerprint:
		return true
	default:
		return false
	}
}

// BuildLabels merges custom labels with container-go tracking labels.
// Reserved tracking keys are taken only from cfg so a caller cannot forge
// ownership or generation metadata through Custom.
//
// LabelReuseGroup is emitted only when Reuse is true. A caller that accepts a
// group without reuse must reject that combination before building labels.
func BuildLabels(cfg LabelConfig) map[string]string {
	labels := make(map[string]string, len(cfg.Custom)+7)
	for k, v := range cfg.Custom {
		if IsReservedLabel(k) {
			continue
		}
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
		if cfg.ReuseConfig != "" {
			labels[LabelReuseConfig] = cfg.ReuseConfig
		}
		if cfg.ReuseEnvFingerprint != "" {
			labels[LabelReuseEnvFingerprint] = cfg.ReuseEnvFingerprint
		}
	}
	return labels
}
