package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestFromSettings(t *testing.T) {
	const rev = "4f2a9c1e0b7d3a8f6e5c2b1a0d9e8f7c6b5a4d3e"
	const stamped = "9b8a7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b"
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		stamp    string
		want     string
	}{
		{"no vcs info", nil, "", "unknown"},
		{"clean tree", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "false"}}, "", rev},
		{"dirty tree", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "true"}}, "", rev + "-dirty"},
		{"unrelated keys ignored", []debug.BuildSetting{{Key: "GOOS", Value: "linux"}, {Key: "vcs.revision", Value: rev}}, "", rev},
		{"stamp without vcs info", []debug.BuildSetting{{Key: "GOOS", Value: "linux"}}, stamped, stamped},
		{"vcs info beats the stamp", []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: "true"}}, stamped, rev + "-dirty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fromSettings(tt.settings, tt.stamp).String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
