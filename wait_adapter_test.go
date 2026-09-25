package container

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/wait"
)

func TestWaitTargetReportsUndeclaredDefaultPort(t *testing.T) {
	target := waitTarget{c: &Container{}}
	_, err := target.Endpoint(context.Background(), "")
	if err == nil {
		t.Fatal("want undeclared-port error")
	}
	if !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("error = %v, want ErrPortNotExposed", err)
	}
}

func TestWaitUndeclaredPortErrorRemainsMatchable(t *testing.T) {
	target := waitTarget{c: &Container{}}
	err := wait.ForListeningPort("6379/tcp").
		WithStartupTimeout(time.Second).
		WaitUntilReady(context.Background(), target)
	if err == nil || !errors.Is(err, ErrPortNotExposed) {
		t.Fatalf("error = %v, want ErrPortNotExposed", err)
	}
}
