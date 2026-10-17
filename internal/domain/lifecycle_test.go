package domain

import (
	"testing"
)

func TestReuseAction(t *testing.T) {
	tests := []struct {
		state     State
		recreated bool
		want      Action
		wantErr   bool
	}{
		{state: StateRunning, want: ActionAttach},
		{state: StateCreated, want: ActionWait},
		{state: StateRestarting, want: ActionWait},
		{state: StateUnknown, want: ActionWait},
		{state: StateStopping, wantErr: true},
		{state: StatePaused, wantErr: true},
		{state: StateStopped, recreated: false, want: ActionRecreate},
		{state: StateStopped, recreated: true, wantErr: true},
	}

	for _, tt := range tests {
		got, err := ReuseAction(tt.state, tt.recreated)
		if (err != nil) != tt.wantErr {
			t.Errorf("ReuseAction(%q, %v) error = %v, wantErr %v", tt.state, tt.recreated, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ReuseAction(%q, %v) = %v, want %v", tt.state, tt.recreated, got, tt.want)
		}
	}
}

func TestCanTransition(t *testing.T) {
	if !CanTransition(StateCreated, StateRunning) {
		t.Error("expected Created -> Running to be valid")
	}
	if !CanTransition(StateRunning, StateStopped) {
		t.Error("expected Running -> Stopped to be valid")
	}
	if !CanTransition(StateStopped, StateRunning) {
		t.Error("expected Stopped -> Running to be valid")
	}
	if CanTransition(StateStopping, StateRunning) {
		t.Error("expected Stopping -> Running to be invalid")
	}
	if !CanTransition(StateRunning, StateRunning) {
		t.Error("expected same-state transition to be valid")
	}
}
