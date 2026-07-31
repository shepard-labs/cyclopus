package service

import (
	"testing"
)

func TestProductionHostname(t *testing.T) {
	tests := []struct {
		name          string
		hostnameLabel string
		suffix        string
		want          string
		wantErr       bool
	}{
		{
			name:          "assembles label and suffix",
			hostnameLabel: "api",
			suffix:        "203-0-113-7.example.test",
			want:          "api.203-0-113-7.example.test",
		},
		{
			name:          "empty suffix returns error",
			hostnameLabel: "api",
			suffix:        "",
			wantErr:       true,
		},
		{
			name:          "empty label returns error",
			hostnameLabel: "",
			suffix:        "203-0-113-7.example.test",
			wantErr:       true,
		},
		{
			name:          "multi-label hostname",
			hostnameLabel: "my-app-2",
			suffix:        "apps.example.com",
			want:          "my-app-2.apps.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := productionHostname(tt.hostnameLabel, tt.suffix)
			if (err != nil) != tt.wantErr {
				t.Fatalf("productionHostname() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("productionHostname() = %q, want %q", got, tt.want)
			}
		})
	}
}
