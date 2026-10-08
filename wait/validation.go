package wait

import (
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strconv"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
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

func isPermanentCheckError(err error) bool {
	if err == nil {
		return false
	}
	var fatal fatalCheckError
	if errors.As(err, &fatal) {
		return true
	}
	var fatalPointer *fatalCheckError
	if errors.As(err, &fatalPointer) && fatalPointer != nil {
		return true
	}
	if errors.Is(err, ErrPortNotExposed) ||
		errors.Is(err, ErrContainerNotFound) ||
		errors.Is(err, ErrTargetNotFound) ||
		errors.Is(err, ErrLogStreamSetup) ||
		errors.Is(err, ErrInvalidConfiguration) ||
		errors.Is(err, errLogLineTooLong) {
		return true
	}
	if cli.PermanentStartError(err) {
		return true
	}
	var launchErr *exec.Error
	return errors.As(err, &launchErr)
}

// isTerminalStreamError identifies a backend process that has already
// exited unsuccessfully. Such an error cannot be repaired by reconnecting
// a logs --follow stream, and must remain visible to the caller.
func isTerminalStreamError(err error) bool {
	if err == nil {
		return false
	}
	var cliErr *cli.CLIError
	return errors.As(err, &cliErr)
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

type strategyValidator interface {
	validate() error
}

// Validate checks a strategy before a container is pulled or created.
// Composite strategies are checked recursively, including typed-nil
// strategy values held in a non-nil interface.
func Validate(strategy Strategy) error {
	return validateStrategy(strategy)
}

// ValidateWithPorts additionally checks the built-in port strategies
// against the ports that Run will declare or publish. It is useful at
// the public container.Run boundary, where the target declarations are
// known before an image is inspected.
func ValidateWithPorts(strategy Strategy, ports []string) error {
	if err := Validate(strategy); err != nil {
		return err
	}
	allowed := make(map[string]struct{}, len(ports))
	for _, port := range ports {
		if err := validatePortSpec(port, false); err != nil {
			return err
		}
		canonical, err := canonicalPortSpec(port)
		if err != nil {
			return err
		}
		allowed[canonical] = struct{}{}
	}
	return validateStrategyPorts(strategy, allowed)
}

func validateStrategy(strategy Strategy) error {
	if isNilStrategy(strategy) {
		return invalidConfigf("strategy must not be nil")
	}
	validator, ok := strategy.(strategyValidator)
	if !ok {
		return nil
	}
	return validator.validate()
}

func isNilStrategy(strategy Strategy) bool {
	if strategy == nil {
		return true
	}
	value := reflect.ValueOf(strategy)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func validateStrategies(strategies []Strategy) error {
	for i, strategy := range strategies {
		if err := validateStrategy(strategy); err != nil {
			return fmt.Errorf("strategy %d: %w", i, err)
		}
	}
	return nil
}

func canonicalPortSpec(port string) (string, error) {
	if err := validatePortSpec(port, false); err != nil {
		return "", err
	}
	portPart, proto, hasProto := strings.Cut(port, "/")
	if !hasProto {
		proto = "tcp"
	}
	n, err := strconv.Atoi(portPart)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(n) + "/" + proto, nil
}

func validateStrategyPorts(strategy Strategy, allowed map[string]struct{}) error {
	switch s := strategy.(type) {
	case *HostPortStrategy:
		if s.portSet {
			return requireWaitPort(allowed, s.port)
		}
		return requireWaitTCPPort(allowed)
	case *HTTPStrategy:
		if !s.portSet {
			return requireWaitTCPPort(allowed)
		}
		return requireWaitPort(allowed, s.port)
	case *AllStrategy:
		for i, child := range s.strategies {
			if err := validateStrategyPorts(child, allowed); err != nil {
				return fmt.Errorf("strategy %d: %w", i, err)
			}
		}
	case *AnyStrategy:
		for i, child := range s.strategies {
			if err := validateStrategyPorts(child, allowed); err != nil {
				return fmt.Errorf("strategy %d: %w", i, err)
			}
		}
	}
	return nil
}

func requireWaitPort(allowed map[string]struct{}, port string) error {
	canonical, err := canonicalPortSpec(port)
	if err != nil {
		return err
	}
	if _, ok := allowed[canonical]; !ok {
		return fmt.Errorf("%w: %s", ErrPortNotExposed, port)
	}
	return nil
}

func requireWaitTCPPort(allowed map[string]struct{}) error {
	for port := range allowed {
		if strings.HasSuffix(port, "/tcp") {
			return nil
		}
	}
	return ErrPortNotExposed
}
