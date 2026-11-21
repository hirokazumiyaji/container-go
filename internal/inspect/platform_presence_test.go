package inspect

import (
	"encoding/json"
	"testing"
)

func TestPlatformUnmarshalPresence(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		want    Platform
		wantErr bool
	}{
		{
			name: "object",
			data: `{"os":"linux","architecture":"arm64","variant":"v8"}`,
			want: Platform{OS: "linux", Architecture: "arm64", Variant: "v8", OSPresent: true, ArchPresent: true, VariantPresent: true},
		},
		{
			name: "omitted fields",
			data: `{}`,
			want: Platform{},
		},
		{
			name: "explicit empty fields",
			data: `{"os":"","architecture":"","variant":""}`,
			want: Platform{OSPresent: true, ArchPresent: true, VariantPresent: true},
		},
		{
			name: "null fields",
			data: `{"os":null,"architecture":null,"variant":null}`,
			want: Platform{OSPresent: true, ArchPresent: true, VariantPresent: true},
		},
		{
			name: "unknown fields",
			data: `{"os":"linux","future":true}`,
			want: Platform{OS: "linux", OSPresent: true},
		},
		{
			// encoding/json matches struct fields case-insensitively, so a
			// present value now implies present metadata. The previous map
			// key check reported this field as both valued and absent.
			name: "case-insensitive key",
			data: `{"OS":"linux"}`,
			want: Platform{OS: "linux", OSPresent: true},
		},
		{
			name: "null document",
			data: `null`,
			want: Platform{},
		},
		{
			name:    "wrong field type",
			data:    `{"os":42}`,
			wantErr: true,
		},
		{
			name:    "not an object",
			data:    `["linux"]`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Platform
			err := json.Unmarshal([]byte(tc.data), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Unmarshal(%s) = %+v, want error", tc.data, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s): %v", tc.data, err)
			}
			if got != tc.want {
				t.Fatalf("Unmarshal(%s) = %+v, want %+v", tc.data, got, tc.want)
			}
		})
	}
}
