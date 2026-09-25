package wait

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func invalidConfigf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfiguration, fmt.Sprintf(format, args...))
}

func (o options) validate() error {
	if o.startupTimeout < 0 {
		return invalidConfigf("startup timeout must not be negative")
	}
	if o.pollInterval < 0 {
		return invalidConfigf("poll interval must not be negative")
	}
	return nil
}

func validatePortSpec(port string, allowEmpty bool) error {
	if port == "" {
		if allowEmpty {
			return nil
		}
		return invalidConfigf("port must not be empty")
	}

	portPart, proto, hasProto := strings.Cut(port, "/")
	if hasProto && proto != "tcp" && proto != "udp" {
		return invalidConfigf("invalid port %q: protocol must be tcp or udp", port)
	}
	n, err := strconv.Atoi(portPart)
	if err != nil || n < 1 || n > 65535 {
		return invalidConfigf("invalid port %q: port must be 1-65535", port)
	}
	return nil
}

func validateTCPPortSpec(port string, allowEmpty bool) error {
	if err := validatePortSpec(port, allowEmpty); err != nil {
		return err
	}
	if port == "" {
		return nil
	}
	_, proto, hasProto := strings.Cut(port, "/")
	if hasProto && proto != "tcp" {
		return invalidConfigf("invalid port %q: wait probes require tcp", port)
	}
	return nil
}

func isPermanentCheckError(err error) bool {
	if err == nil {
		return false
	}
	var fatal fatalCheckError
	if errors.As(err, &fatal) {
		return true
	}
	var fatalPointer *fatalCheckError
	if errors.As(err, &fatalPointer) {
		return true
	}
	return errors.Is(err, ErrPortNotExposed) ||
		errors.Is(err, ErrContainerNotFound) ||
		errors.Is(err, ErrInvalidConfiguration)
}

func validateHTTPHeader(key, value string) error {
	if !validHeaderFieldName(key) {
		return invalidConfigf("invalid HTTP header name %q", key)
	}
	for i := 0; i < len(value); i++ {
		if value[i] == '\t' {
			continue
		}
		if value[i] < 0x20 || value[i] == 0x7f {
			return invalidConfigf("invalid HTTP header value for %q", key)
		}
	}
	return nil
}

func validHeaderFieldName(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)):
		default:
			return false
		}
	}
	return true
}

func validateDuration(name string, d time.Duration) error {
	if d < 0 {
		return invalidConfigf("%s must not be negative", name)
	}
	return nil
}

type strategyValidator interface {
	validate() error
}

func validateStrategies(strategies []Strategy) error {
	for i, strategy := range strategies {
		if strategy == nil {
			return invalidConfigf("strategy %d must not be nil", i)
		}
		validator, ok := strategy.(strategyValidator)
		if !ok {
			continue
		}
		if err := validator.validate(); err != nil {
			return fmt.Errorf("strategy %d: %w", i, err)
		}
	}
	return nil
}
