package container

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"
)

const maxStopTestDuration = time.Duration(1<<63 - 1)

type stopArgsBuilder func(string, *time.Duration) ([]string, error)

func TestDockerStopArgs(t *testing.T) {
	testStopArgs(t, dockerEngine{}.stopArgs)
}

func TestAppleStopArgs(t *testing.T) {
	testStopArgs(t, appleEngine{}.stopArgs)
}

func testStopArgs(t *testing.T, build stopArgsBuilder) {
	t.Helper()

	maxSeconds := int64(maxStopTestDuration / time.Second)
	if maxStopTestDuration%time.Second != 0 {
		maxSeconds++
	}
	maxArg := strconv.FormatInt(maxSeconds, 10)
	maxOverflows := strconv.IntSize == 32
	cases := []struct {
		name        string
		nilTimeout  bool
		timeout     time.Duration
		wantSeconds string
		wantErr     bool
	}{
		{name: "nil omits time", nilTimeout: true},
		{name: "zero", wantSeconds: "0"},
		{name: "one nanosecond", timeout: time.Nanosecond, wantSeconds: "1"},
		{name: "sub-second", timeout: 999 * time.Millisecond, wantSeconds: "1"},
		{name: "whole second", timeout: time.Second, wantSeconds: "1"},
		{name: "fractional second", timeout: 1500 * time.Millisecond, wantSeconds: "2"},
		{name: "negative", timeout: -time.Second, wantErr: true},
		{name: "maximum", timeout: maxStopTestDuration, wantSeconds: maxArg, wantErr: maxOverflows},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var timeout *time.Duration
			if !tc.nilTimeout {
				timeout = &tc.timeout
			}
			got, err := build("myctr", timeout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("stopArgs = %v, want error", got)
				}
				if got != nil {
					t.Errorf("stopArgs = %v, want nil on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("stopArgs: %v", err)
			}
			want := []string{"stop"}
			if !tc.nilTimeout {
				want = append(want, "--time", tc.wantSeconds)
			}
			want = append(want, "myctr")
			if !slices.Equal(got, want) {
				t.Errorf("stopArgs = %v, want %v", got, want)
			}
		})
	}
}

func TestStopTimeoutConversionRejectsOverflow(t *testing.T) {
	if _, err := stopTimeoutSeconds(2*time.Second, 1); err == nil {
		t.Fatal("stopTimeoutSeconds accepted seconds above the backend limit")
	}
}

func TestStopTimeout(t *testing.T) {
	maxSeconds := int64(maxStopTestDuration / time.Second)
	if maxStopTestDuration%time.Second != 0 {
		maxSeconds++
	}
	maxArg := strconv.FormatInt(maxSeconds, 10)
	maxOverflows := strconv.IntSize == 32

	cases := []struct {
		name        string
		nilTimeout  bool
		timeout     time.Duration
		wantFlag    bool
		wantSeconds string
		wantErr     bool
	}{
		{name: "nil uses backend default", nilTimeout: true},
		{name: "zero requests immediate stop", wantFlag: true, wantSeconds: "0"},
		{name: "one nanosecond rounds up", timeout: time.Nanosecond, wantFlag: true, wantSeconds: "1"},
		{name: "sub-second rounds up", timeout: 999 * time.Millisecond, wantFlag: true, wantSeconds: "1"},
		{name: "one second is unchanged", timeout: time.Second, wantFlag: true, wantSeconds: "1"},
		{name: "fractional second rounds up", timeout: 1500 * time.Millisecond, wantFlag: true, wantSeconds: "2"},
		{name: "negative is rejected", timeout: -time.Second, wantErr: true},
		{name: "maximum duration", timeout: maxStopTestDuration, wantFlag: !maxOverflows, wantSeconds: maxArg, wantErr: maxOverflows},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestRunner()
			ctr := runTestContainer(t, f)
			f.calls = nil

			var timeout *time.Duration
			if !tc.nilTimeout {
				timeout = &tc.timeout
			}
			err := ctr.Stop(context.Background(), timeout)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Stop returned nil error")
				}
				if stop := f.callWith("stop"); stop != nil {
					t.Fatalf("invalid timeout reached backend: %v", stop)
				}
				return
			}
			if err != nil {
				t.Fatalf("Stop: %v", err)
			}

			want := []string{"stop"}
			if tc.wantFlag {
				want = append(want, "--time", tc.wantSeconds)
			}
			want = append(want, "myctr")
			if got := f.callWith("stop"); !slices.Equal(got, want) {
				t.Errorf("stop args = %v, want %v", got, want)
			}
		})
	}
}

type stopContextRunner struct {
	deadline    time.Time
	hasDeadline bool
	contextErr  error
}

func (r *stopContextRunner) Run(ctx context.Context, _ ...string) ([]byte, []byte, error) {
	r.deadline, r.hasDeadline = ctx.Deadline()
	r.contextErr = ctx.Err()
	return nil, nil, nil
}

func TestStopMaximumTimeoutSaturatesContextBudget(t *testing.T) {
	if strconv.IntSize == 32 {
		t.Skip("maximum duration overflows the backend integer")
	}
	runner := new(stopContextRunner)
	ctr := &Container{id: "myctr", runner: runner, eng: appleEngine{}}
	timeout := maxStopTestDuration
	start := time.Now()

	if err := ctr.Stop(context.Background(), &timeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !runner.hasDeadline {
		t.Fatal("stop context has no deadline")
	}
	if runner.contextErr != nil {
		t.Fatalf("stop context already ended: %v", runner.contextErr)
	}
	budget := runner.deadline.Sub(start)
	if budget > maxStopTestDuration || budget < maxStopTestDuration-time.Second {
		t.Errorf("stop context budget = %v, want approximately %v", budget, maxStopTestDuration)
	}
}
