package domain

import "testing"

func TestDecideReuse(t *testing.T) {
	tests := []struct {
		name string
		in   ReuseInput
		want ReuseDecision
	}{
		{
			name: "absent",
			in:   ReuseInput{},
			want: ReuseDecision{Action: ReuseCreateNew, Reason: "no container exists for the name"},
		},
		{
			name: "running",
			in:   ReuseInput{Exists: true, State: StateRunning, OwnershipVerified: true, ImageCompatible: true},
			want: ReuseDecision{Action: ReuseAttachRunning, Reason: "container is running"},
		},
		{
			name: "stopped",
			in:   ReuseInput{Exists: true, State: StateStopped, OwnershipVerified: true, ImageCompatible: true},
			want: ReuseDecision{Action: ReuseRecreateIncompatible, Reason: "container is stopped"},
		},
		{
			name: "stopped after recreate",
			in:   ReuseInput{Exists: true, State: StateStopped, OwnershipVerified: true, ImageCompatible: true, Recreated: true},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container stayed stopped after recreate"},
		},
		{
			name: "created",
			in:   ReuseInput{Exists: true, State: StateCreated, OwnershipVerified: true, ImageCompatible: true},
			want: ReuseDecision{Action: ReuseStartExisting, Reason: "container is starting"},
		},
		{
			name: "restarting",
			in:   ReuseInput{Exists: true, State: StateRestarting, OwnershipVerified: true, ImageCompatible: true},
			want: ReuseDecision{Action: ReuseStartExisting, Reason: "container is starting"},
		},
		{
			name: "unknown state",
			in:   ReuseInput{Exists: true, State: StateUnknown, OwnershipVerified: true, ImageCompatible: true},
			want: ReuseDecision{Action: ReuseStartExisting, Reason: "container is starting"},
		},
		{
			name: "ownership mismatch",
			in:   ReuseInput{Exists: true, State: StateRunning, OwnershipVerified: false, ImageCompatible: true},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container ownership does not match"},
		},
		{
			name: "image mismatch",
			in:   ReuseInput{Exists: true, State: StateRunning, OwnershipVerified: true, ImageCompatible: false},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container image identity does not match"},
		},
		{
			name: "stopped ownership mismatch",
			in:   ReuseInput{Exists: true, State: StateStopped, OwnershipVerified: false, ImageCompatible: true},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container ownership does not match"},
		},
		{
			name: "stopped image mismatch",
			in:   ReuseInput{Exists: true, State: StateStopped, OwnershipVerified: true, ImageCompatible: false},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container image identity does not match"},
		},
		{
			name: "stopping",
			in:   ReuseInput{Exists: true, State: StateStopping, OwnershipVerified: true, ImageCompatible: true},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container is stopping and cannot become ready"},
		},
		{
			name: "paused",
			in:   ReuseInput{Exists: true, State: StatePaused, OwnershipVerified: true, ImageCompatible: true},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container is paused and cannot become ready"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecideReuse(tt.in); got != tt.want {
				t.Errorf("DecideReuse(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDecideReuseCompat(t *testing.T) {
	tests := []struct {
		name string
		in   ReuseCompatInput
		want ReuseDecision
	}{
		{
			name: "generation replaced",
			in: ReuseCompatInput{
				State:              StateRunning,
				GenerationVerified: false,
				OwnershipVerified:  true,
				ImageCompatible:    true,
				ConfigCompatible:   true,
			},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container generation does not match"},
		},
		{
			name: "stopped generation replaced",
			in: ReuseCompatInput{
				State:              StateStopped,
				GenerationVerified: false,
			},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container generation does not match"},
		},
		{
			name: "running compatible",
			in: ReuseCompatInput{
				State:              StateRunning,
				GenerationVerified: true,
				OwnershipVerified:  true,
				ImageCompatible:    true,
				ConfigCompatible:   true,
			},
			want: ReuseDecision{Action: ReuseAttachRunning, Reason: "container is running"},
		},
		{
			name: "ownership mismatch",
			in: ReuseCompatInput{
				State:              StateRunning,
				GenerationVerified: true,
				OwnershipVerified:  false,
				ImageCompatible:    true,
				ConfigCompatible:   true,
			},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container ownership does not match"},
		},
		{
			name: "image mismatch",
			in: ReuseCompatInput{
				State:              StateRunning,
				GenerationVerified: true,
				OwnershipVerified:  true,
				ImageCompatible:    false,
				ConfigCompatible:   true,
			},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container image identity does not match"},
		},
		{
			name: "config mismatch",
			in: ReuseCompatInput{
				State:              StateRunning,
				GenerationVerified: true,
				OwnershipVerified:  true,
				ImageCompatible:    true,
				ConfigCompatible:   false,
			},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container configuration does not match"},
		},
		{
			name: "port pending",
			in: ReuseCompatInput{
				State:              StateRunning,
				GenerationVerified: true,
				OwnershipVerified:  true,
				ImageCompatible:    true,
				ConfigCompatible:   false,
				PortPending:        true,
			},
			want: ReuseDecision{Action: ReuseStartExisting, Reason: "requested port binding is not published yet"},
		},
		{
			name: "address not reported",
			in: ReuseCompatInput{
				State:              StateRunning,
				GenerationVerified: true,
				OwnershipVerified:  true,
				ImageCompatible:    true,
				ConfigCompatible:   true,
				WaitForAddress:     true,
			},
			want: ReuseDecision{Action: ReuseStartExisting, Reason: "container address is not reported yet"},
		},
		{
			name: "created",
			in: ReuseCompatInput{
				State:              StateCreated,
				GenerationVerified: true,
			},
			want: ReuseDecision{Action: ReuseStartExisting, Reason: "container state is not final"},
		},
		{
			name: "stopped",
			in: ReuseCompatInput{
				State:              StateStopped,
				GenerationVerified: true,
				OwnershipVerified:  true,
				ImageCompatible:    true,
				ConfigCompatible:   true,
			},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container stopped before becoming ready"},
		},
		{
			name: "stopping",
			in: ReuseCompatInput{
				State:              StateStopping,
				GenerationVerified: true,
			},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container is stopping and cannot become ready"},
		},
		{
			name: "paused",
			in: ReuseCompatInput{
				State:              StatePaused,
				GenerationVerified: true,
			},
			want: ReuseDecision{Action: ReuseFailMismatch, Reason: "container is paused and cannot become ready"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecideReuseCompat(tt.in); got != tt.want {
				t.Errorf("DecideReuseCompat(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}
