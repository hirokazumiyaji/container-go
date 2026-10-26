package container

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hirokazumiyaji/container-go/wait"
)

// TestPublicErrorTypesDoNotExposeAsRedacted keeps the pre-1.0 API from
// exposing internal/diagnostic.Redactor through a public method. Public error
// types still redact through the registered provider in redaction.go.
func TestPublicErrorTypesDoNotExposeAsRedacted(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeOf(ConfigError{}),
		reflect.TypeOf(ValidationError{}),
		reflect.TypeOf(ExecTerminationError{}),
		reflect.TypeOf(CleanupError{}),
		reflect.TypeOf(CLIError{}),
		reflect.TypeOf(SystemNotRunningError{}),
		reflect.TypeOf(wait.ConfigError{}),
	}
	for _, typ := range types {
		ptr := reflect.PointerTo(typ)
		if _, ok := ptr.MethodByName("AsRedacted"); ok {
			t.Errorf("%s still exposes AsRedacted", ptr)
		}
		for i := 0; i < ptr.NumMethod(); i++ {
			method := ptr.Method(i)
			if strings.Contains(method.Type.String(), "/internal/") {
				t.Errorf("%s.%s exposes an internal type: %s", ptr, method.Name, method.Type)
			}
		}
	}
}

func TestLifecycleHelpersTakeCallerContext(t *testing.T) {
	if got := reflect.TypeOf(TerminateContainer).String(); got != "func(context.Context, *container.Container) error" {
		t.Errorf("TerminateContainer type = %s", got)
	}
	if got := reflect.TypeOf((*Container).Kill).String(); got != "func(*container.Container, context.Context, string) error" {
		t.Errorf("Container.Kill type = %s", got)
	}
}
