package container

import (
	"context"
	"math"
	"slices"
	"strconv"
	"testing"
	"time"
)

const (
	maxStopTestDuration      time.Duration = math.MaxInt64
	maxAppleStopTestSeconds  int64         = math.MaxInt32
	maxDockerStopTestSeconds int64         = math.MaxInt32
)

type stopArgsBuilder func(string, *time.Duration) ([]string, error)

func TestDockerStopArgs(t *testing.T) {
	testStopArgs(t, dockerEngine{}.stopArgs, maxDockerStopTestSeconds)
}

func TestDockerStopArgs32BitBoundary(t *testing.T) {
	const wantMax int64 = math.MaxInt32
	if maxDockerStopSeconds != wantMax {
		t.Fatalf("maxDockerStopSeconds = %d, want %d", maxDockerStopSeconds, wantMax)
	}

	maxTimeout := time.Duration(wantMax) * time.Second
	got, err := (dockerEngine{}).stopArgs("myctr", &maxTimeout)
	if err != nil {
		t.Fatalf("maximum 32-bit timeout: %v", err)
	}
	want := []string{"stop", "--time", strconv.FormatInt(wantMax, 10), "myctr"}
	if !slices.Equal(got, want) {
		t.Errorf("stopArgs = %v, want %v", got, want)
	}

	firstInvalid := maxTimeout + time.Nanosecond
	if got, err := (dockerEngine{}).stopArgs("myctr", &firstInvalid); err == nil {
		t.Fatalf("accepted first timeout above the 32-bit limit: %v", got)
	}
}

func TestAppleStopArgs(t *testing.T) {
	testStopArgs(t, appleEngine{}.stopArgs, maxAppleStopTestSeconds)
}

func testStopArgs(t *testing.T, build stopArgsBuilder, maxSeconds int64) {
	t.Helper()

	maxTimeout := time.Duration(maxSeconds) * time.Second
	maxArg := strconv.FormatInt(maxSeconds, 10)
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
		{name: "backend maximum", timeout: maxTimeout, wantSeconds: maxArg},
		{name: "first value rounding above maximum", timeout: maxTimeout + time.Nanosecond, wantErr: true},
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

func TestStopTimeoutBackendBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		maxSeconds int64
	}{
		{name: "Apple", maxSeconds: maxAppleStopTestSeconds},
		{name: "Docker", maxSeconds: maxDockerStopTestSeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			maxTimeout := time.Duration(tc.maxSeconds) * time.Second
			firstInvalid := maxTimeout + time.Nanosecond

			got, err := stopTimeoutSeconds(maxTimeout, tc.maxSeconds)
			if err != nil {
				t.Fatalf("maximum safe timeout: %v", err)
			}
			if got != tc.maxSeconds {
				t.Errorf("maximum safe seconds = %d, want %d", got, tc.maxSeconds)
			}
			if _, err := stopTimeoutSeconds(firstInvalid, tc.maxSeconds); err == nil {
				t.Fatalf("accepted first timeout that rounds above maximum: %s", firstInvalid)
			}
			if _, err := stopTimeoutSeconds(maxStopTestDuration, tc.maxSeconds); err == nil {
				t.Fatal("accepted maximum time.Duration")
			}
		})
	}
}

func TestStopTimeout(t *testing.T) {
	backends := []struct {
		name       string
		engine     engine
		maxSeconds int64
	}{
		{name: "Apple", engine: appleEngine{}, maxSeconds: maxAppleStopTestSeconds},
		{name: "Docker", engine: dockerEngine{}, maxSeconds: maxDockerStopTestSeconds},
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			maxTimeout := time.Duration(backend.maxSeconds) * time.Second
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
				{name: "backend maximum", timeout: maxTimeout, wantFlag: true, wantSeconds: strconv.FormatInt(backend.maxSeconds, 10)},
				{name: "first value rounding above maximum", timeout: maxTimeout + time.Nanosecond, wantErr: true},
				{name: "maximum duration", timeout: maxStopTestDuration, wantErr: true},
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					ctr, runner := newStopTestContainer(t, backend.engine)
					runner.calls = nil

					var timeout *time.Duration
					if !tc.nilTimeout {
						timeout = &tc.timeout
					}
					err := ctr.Stop(context.Background(), timeout)
					if tc.wantErr {
						if err == nil {
							t.Fatal("Stop returned nil error")
						}
						if stop := runner.callWith("stop"); stop != nil {
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
					if got := runner.callWith("stop"); !slices.Equal(got, want) {
						t.Errorf("stop args = %v, want %v", got, want)
					}
				})
			}
		})
	}
}

func newStopTestContainer(t *testing.T, eng engine) (*Container, *fakeRunner) {
	t.Helper()
	if _, ok := eng.(dockerEngine); ok {
		runner := &dockerRunner{fakeRunner: newTestRunner()}
		return runDockerTestContainer(t, runner), runner.fakeRunner
	}
	runner := newTestRunner()
	return runTestContainer(t, runner), runner
}

func TestSaturatingAddDuration(t *testing.T) {
	if got := saturatingAddDuration(queryTimeout, maxStopTestDuration); got != maxStopTestDuration {
		t.Errorf("saturatingAddDuration(max) = %v, want %v", got, maxStopTestDuration)
	}
	if got, want := saturatingAddDuration(queryTimeout, -time.Second), queryTimeout-time.Second; got != want {
		t.Errorf("saturatingAddDuration(negative) = %v, want %v", got, want)
	}
}
