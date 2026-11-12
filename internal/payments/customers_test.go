package payments

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptr[T any](v T) *T { return &v }

func TestCreateCustomer(t *testing.T) {
	f := newFixture(t)
	c, err := f.svc.CreateCustomer(t.Context(), f.merchant, ptr("asha@example.com"), ptr("Asha Menon"))
	require.NoError(t, err)
	assert.Regexp(t, `^cus_[A-Za-z0-9]{24}$`, c.ID)
	assert.Equal(t, "asha@example.com", *c.Email)

	got, err := f.svc.GetCustomer(t.Context(), f.merchant, c.ID)
	require.NoError(t, err)
	assert.Equal(t, c, got)

	empty, err := f.svc.CreateCustomer(t.Context(), f.merchant, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, empty.Email)
	assert.Nil(t, empty.Name)
}

func TestCustomerEmailMustBeAPlainAddress(t *testing.T) {
	f := newFixture(t)
	for _, email := range []string{"", "asha", "Asha <asha@example.com>", "asha@example.com ", strings.Repeat("a", 501) + "@example.com"} {
		_, err := f.svc.CreateCustomer(t.Context(), f.merchant, &email, nil)
		requireAPIError(t, err, 400, "email_invalid", "email")
	}
	_, err := f.svc.CreateCustomer(t.Context(), f.merchant, nil, ptr(strings.Repeat("ā", 257)))
	requireAPIError(t, err, 400, "parameter_invalid", "name")
}

// Walk every page while other requests keep inserting: each payment that
// existed at the start is seen exactly once, newest first.
func TestPaginationHasNoDuplicatesOrGapsWhileInserting(t *testing.T) {
	f := newFixture(t)
	var want []string
	for range 25 {
		want = append([]string{f.create(t, "").ID}, want...)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, err := f.svc.Create(t.Context(), f.merchant, CreateParams{Amount: 1, Currency: "inr"})
				assert.NoError(t, err)
			}
		}
	})

	page, seen := Page{Limit: 4}, map[string]bool{}
	var got []string
	for {
		items, hasMore, err := f.svc.List(t.Context(), f.merchant, page)
		require.NoError(t, err)
		for _, pi := range items {
			require.False(t, seen[pi.ID], "%s appeared twice", pi.ID)
			seen[pi.ID] = true
			if pi.Amount == 49900 {
				got = append(got, pi.ID)
			}
		}
		if !hasMore {
			break
		}
		page.StartingAfter = items[len(items)-1].ID
	}
	close(stop)
	wg.Wait()
	assert.Equal(t, want, got)
}

func TestListPagesAndReportsHasMore(t *testing.T) {
	f := newFixture(t)
	var ids []string
	for range 3 {
		c, err := f.svc.CreateCustomer(t.Context(), f.merchant, nil, nil)
		require.NoError(t, err)
		ids = append([]string{c.ID}, ids...)
	}

	first, more, err := f.svc.ListCustomers(t.Context(), f.merchant, Page{Limit: 2})
	require.NoError(t, err)
	assert.True(t, more)
	require.Len(t, first, 2)
	assert.Equal(t, ids[:2], []string{first[0].ID, first[1].ID})

	rest, more, err := f.svc.ListCustomers(t.Context(), f.merchant, Page{Limit: 2, StartingAfter: first[1].ID})
	require.NoError(t, err)
	assert.False(t, more)
	require.Len(t, rest, 1)
	assert.Equal(t, ids[2], rest[0].ID)

	_, _, err = f.svc.ListCustomers(t.Context(), f.merchant, Page{Limit: 2, StartingAfter: "cus_missing"})
	requireAPIError(t, err, 400, "resource_missing", "starting_after")
}
