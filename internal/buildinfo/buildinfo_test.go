package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestFromSettings(t *testing.T) {
	const rev = "4f2a9c1e0b7d3a8f6e5c2b1a0d9e8f7c6b5a4d3e"
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{"no vcs info", nil, "unknown"},
		{"clean tree", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "false"}}, rev},
		{"dirty tree", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "true"}}, rev + "-dirty"},
		{"unrelated keys ignored", []debug.BuildSetting{{Key: "GOOS", Value: "linux"}, {Key: "vcs.revision", Value: rev}}, rev},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fromSettings(tt.settings).String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
