package container

import "testing"

func TestParseInspectHealthStatus(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  string
	}{
		{
			name:  "starting",
			state: `{"Status":"running","Health":{"Status":"starting"}}`,
			want:  "starting",
		},
		{
			name:  "healthy",
			state: `{"Status":"running","Health":{"Status":"healthy"}}`,
			want:  "healthy",
		},
		{
			name:  "no healthcheck is null",
			state: `{"Status":"running","Health":null}`,
			want:  "",
		},
		{
			name:  "no healthcheck is absent",
			state: `{"Status":"running"}`,
			want:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(`[{"Id":"` + dockerFixtureID + `","Name":"/myctr","State":` + tc.state + `}]`)
			info, err := (dockerEngine{}).parseInspect(data, "myctr")
			if err != nil {
				t.Fatalf("parseInspect: %v", err)
			}
			if info.health != tc.want {
				t.Errorf("health = %q, want %q", info.health, tc.want)
			}
		})
	}
}
