package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Arjunx156/ledgerline/internal/apierror"
)

// Page selects one page of a list (FR-12): at most Limit objects, starting
// after the object with id StartingAfter, if set.
type Page struct {
	Limit         int
	StartingAfter string
}

// list pages through a merchant's rows newest first, keyed on
// (created_at, id). The key comes from the cursor row itself, so rows
// inserted while a client pages land before its cursor: it never sees one
// twice or skips one that was there when it started.
//
// table, object and columns are constants from this package, never input.
func list[T any](ctx context.Context, db *pgxpool.Pool, table, object, columns, merchantID string, page Page,
	scan func(pgx.Row) (T, error)) ([]T, bool, error) {
	where, args := `merchant_id = $1`, []any{merchantID, page.Limit + 1}
	if page.StartingAfter != "" {
		var created time.Time
		err := db.QueryRow(ctx, `SELECT created_at FROM `+table+` WHERE merchant_id = $1 AND id = $2`,
			merchantID, page.StartingAfter).Scan(&created)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, apierror.NoSuch("starting_after", object, page.StartingAfter)
		}
		if err != nil {
			return nil, false, fmt.Errorf("list %s: find cursor: %w", table, err)
		}
		where += ` AND (created_at, id) < ($3, $4)`
		args = append(args, created, page.StartingAfter)
	}
	rows, err := db.Query(ctx, `SELECT `+columns+` FROM `+table+` WHERE `+where+
		` ORDER BY created_at DESC, id DESC LIMIT $2`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list %s: %w", table, err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (T, error) { return scan(row) })
	if err != nil {
		return nil, false, fmt.Errorf("list %s: %w", table, err)
	}
	if len(items) > page.Limit {
		return items[:page.Limit], true, nil
	}
	return items, false, nil
}
