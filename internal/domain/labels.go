package domain

// Label constants applied by container-go to track managed containers.
const (
	LabelManaged    = "com.github.hirokazumiyaji.container-go"
	LabelSession    = "com.github.hirokazumiyaji.container-go.session"
	LabelCreation   = "com.github.hirokazumiyaji.container-go.creation"
	LabelReuse      = "com.github.hirokazumiyaji.container-go.reuse"
	LabelReuseGroup = "com.github.hirokazumiyaji.container-go.reuse-group"
)

// LabelConfig holds the fields needed to construct standard container-go labels.
type LabelConfig struct {
	Session    string
	Creation   string
	Reuse      bool
	ReuseGroup string
	Custom     map[string]string
}

// reservedLabel reports whether key is owned by container-go. Custom labels
// may not set these keys: the internal value is the only trustworthy source
// for ownership and cleanup decisions.
func reservedLabel(key string) bool {
	switch key {
	case LabelManaged, LabelSession, LabelCreation, LabelReuse, LabelReuseGroup:
		return true
	default:
		return false
	}
}

// BuildLabels merges custom labels with container-go tracking labels.
// Reserved tracking keys are taken only from cfg so a caller cannot forge
// ownership or generation metadata through Custom.
func BuildLabels(cfg LabelConfig) map[string]string {
	labels := make(map[string]string, len(cfg.Custom)+5)
	for k, v := range cfg.Custom {
		if reservedLabel(k) {
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
	}
	return labels
}
