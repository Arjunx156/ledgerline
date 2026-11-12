package main

import (
	"context"
	"fmt"
	"io"

	"github.com/Arjunx156/ledgerline/internal/auth"
	"github.com/Arjunx156/ledgerline/internal/database"
)

// seed creates a merchant and prints its secret key. This is the only time
// the key is ever shown: the database keeps just its SHA-256.
func seed(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	if len(args) > 1 {
		return errUsage{"seed takes at most one argument, the merchant's name"}
	}
	name := "Test merchant"
	if len(args) == 1 {
		name = args[0]
	}
	dbURL, err := requireEnv(getenv, "LEDGERLINE_DATABASE_URL")
	if err != nil {
		return err
	}
	pool, err := database.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	merchantID, key, err := auth.CreateMerchant(ctx, pool, name)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "merchant    %s\nsecret key  %s\n\nCopy the key now: it is not stored and can't be shown again.\n", merchantID, key)
	return nil
}
