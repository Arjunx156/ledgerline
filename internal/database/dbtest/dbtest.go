// Package dbtest gives each test its own freshly migrated Postgres database,
// so tests never share state and can run in parallel.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Arjunx156/ledgerline/internal/database"
)

// Must match deploy/postgres/init/10-app-role.sql.
const (
	appRole     = "ledgerline_app"
	appPassword = "ledgerline_app"
)

// DB is one throwaway database, reachable as the owner and as the app role.
type DB struct {
	OwnerURL string
	Owner    *pgxpool.Pool // migrations and DDL; bypasses grants but not triggers
	App      *pgxpool.Pool // ledgerline_app, the role serve and worker use
}

// New creates a database, applies every migration and drops it when the
// test ends.
func New(t testing.TB) *DB {
	t.Helper()
	adminURL := os.Getenv("LEDGERLINE_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Fatal("LEDGERLINE_TEST_DATABASE_URL is not set: run `just up`, then run tests through `just test` (it loads .env)")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to test postgres (is it up? `just up`): %v", err)
	}
	defer admin.Close(ctx)

	// Roles are cluster-wide and test packages run concurrently, so creation
	// can race; either error just means another package won.
	_, err = admin.Exec(ctx, `DO $$ BEGIN
		CREATE ROLE `+appRole+` LOGIN PASSWORD '`+appPassword+`';
	EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$`)
	if err != nil {
		t.Fatalf("ensure app role: %v", err)
	}

	name := "ledgerline_test_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			t.Errorf("drop test database %s: %v", name, err)
			return
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})

	db := &DB{OwnerURL: withDatabase(t, adminURL, name, nil)}
	if err := database.MigrateUp(db.OwnerURL); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	db.Owner = open(t, db.OwnerURL)
	db.App = open(t, withDatabase(t, adminURL, name, url.UserPassword(appRole, appPassword)))
	return db
}

func open(t testing.TB, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := database.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func withDatabase(t testing.TB, dsn, name string, user *url.Userinfo) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse LEDGERLINE_TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	if user != nil {
		u.User = user
	}
	return u.String()
}

func randomSuffix(t testing.TB) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(b)
}
