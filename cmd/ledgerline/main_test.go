package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func noEnv(string) string { return "" }

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
		{"version with args", []string{"version", "extra"}, 2, "", "takes no arguments"},
		{"serve with args", []string{"serve", "now"}, 2, "", "takes no arguments"},
		{"serve without database", []string{"serve"}, 1, "", "LEDGERLINE_DATABASE_URL is not set"},
		{"migrate alone", []string{"migrate"}, 2, "", "migrate needs up, down or version"},
		{"migrate sideways", []string{"migrate", "sideways"}, 2, "", `unknown migrate command "sideways"`},
		{"migrate down without steps", []string{"migrate", "down"}, 2, "", "needs a number of steps"},
		{"migrate down zero", []string{"migrate", "down", "0"}, 2, "", `got "0"`},
		{"migrate down words", []string{"migrate", "down", "lots"}, 2, "", `got "lots"`},
		{"migrate without database", []string{"migrate", "up"}, 1, "", "LEDGERLINE_MIGRATE_URL is not set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, noEnv, &stdout, &stderr)
			assert.Equal(t, tt.wantCode, code)
			if tt.wantStdout == "" {
				assert.Empty(t, stdout.String())
			} else {
				assert.Contains(t, stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr == "" {
				assert.Empty(t, stderr.String())
			} else {
				assert.Contains(t, stderr.String(), tt.wantStderr)
			}
		})
	}
}

// A shutdown signal mid-request must let the request finish, not cut it off.
func TestRunServerDrainsInFlightRequests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	inHandler := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inHandler)
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "finished")
	})

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { stopped <- runServer(ctx, ln, handler, logger) }()

	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		got <- result{string(b), err}
	}()

	<-inHandler
	cancel()

	r := <-got
	require.NoError(t, r.err)
	assert.Equal(t, "finished", r.body)
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(drainTimeout):
		t.Fatal("server did not stop after draining")
	}

	_, err = http.Get("http://" + ln.Addr().String() + "/")
	require.Error(t, err, "no new connections after shutdown")
}
