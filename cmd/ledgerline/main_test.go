package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"version", []string{"version"}, 0, "ledgerline ", ""},
		{"help", []string{"help"}, 0, "usage: ledgerline", ""},
		{"no command", nil, 2, "", "usage: ledgerline"},
		{"unknown command", []string{"charge"}, 2, "", `unknown command "charge"`},
		{"too many args", []string{"version", "extra"}, 2, "", "usage: ledgerline"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) || (tt.wantStdout == "" && stdout.Len() > 0) {
				t.Errorf("stdout %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) || (tt.wantStderr == "" && stderr.Len() > 0) {
				t.Errorf("stderr %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
