package ids

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewShape(t *testing.T) {
	shape := regexp.MustCompile(`^pi_[A-Za-z0-9]{24}$`)
	for range 1000 {
		id := New(PaymentIntent)
		require.Regexp(t, shape, id)
	}
}

func TestNewIsUnique(t *testing.T) {
	seen := make(map[string]bool, 100_000)
	for range 100_000 {
		id := New(Customer)
		require.False(t, seen[id], "duplicate id %s", id)
		seen[id] = true
	}
}

// Every character of the alphabet should appear about equally often; a
// modulo bias would push the first eight well above the rest.
func TestRandomIsUniform(t *testing.T) {
	counts := map[rune]int{}
	const draws = 62 * 4000
	for _, c := range Random(draws) {
		counts[c]++
	}
	require.Len(t, counts, len(alphabet))
	for c, n := range counts {
		assert.InDelta(t, 4000, n, 400, "character %q", c)
	}
}
