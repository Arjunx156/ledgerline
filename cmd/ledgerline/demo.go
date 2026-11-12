package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/Arjunx156/ledgerline/internal/api"
	"github.com/Arjunx156/ledgerline/internal/demo"
)

const (
	// Amendment 3.13: at most 10 sessions per visitor address an hour.
	sessionsPerHour = 10

	// Well inside the idle timeouts of the proxies in front of a free host.
	streamKeepAlive = 15 * time.Second
)

// runDemo is serve plus the proof console's backend (amendment 3.13), for
// the public deployment: sandbox sessions, and a stream of each session's
// trace fed by Postgres LISTEN.
func runDemo(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	origins, err := demoOrigins(getenv("LEDGERLINE_DEMO_ORIGINS"))
	if err != nil {
		return err
	}
	app, err := start(ctx, "serving demo", getenv, logger)
	if err != nil {
		return err
	}
	defer app.pool.Close()

	rec := demo.NewRecorder(app.pool, logger)
	defer rec.Close() // after the server has drained, so its last events are stored

	hub := demo.NewHub(app.pool.Config().ConnConfig, logger)
	hubCtx, stopHub := context.WithCancel(ctx)
	hubDone := make(chan struct{})
	go func() { hub.Run(hubCtx); close(hubDone) }()
	defer func() { stopHub(); <-hubDone }()

	cfg := app.config(logger)
	cfg.Demo = &api.Demo{
		Recorder:       rec,
		Hub:            hub,
		Sessions:       demo.NewLimiter(sessionsPerHour, time.Hour, time.Now),
		SessionTTL:     demo.SessionTTL,
		Origins:        origins,
		ClientIPHeader: getenv("LEDGERLINE_CLIENT_IP_HEADER"),
		KeepAlive:      streamKeepAlive,
		Shutdown:       ctx.Done(),
	}
	return runServer(ctx, app.ln, api.NewRouter(cfg), logger)
}

// demoOrigins parses the comma-separated sites allowed to call the API from
// a browser. Each must be an origin, scheme and host only, because that is
// what a browser sends.
func demoOrigins(list string) ([]string, error) {
	var origins []string
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
			strings.TrimSuffix(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("LEDGERLINE_DEMO_ORIGINS: %q is not an origin such as https://example.com", raw)
		}
		origins = append(origins, u.Scheme+"://"+u.Host)
	}
	if len(origins) == 0 {
		return nil, fmt.Errorf("LEDGERLINE_DEMO_ORIGINS is not set: list the origins the demo page is served from, " +
			"comma-separated, such as https://ledgerline-skeleton.onrender.com")
	}
	return origins, nil
}
