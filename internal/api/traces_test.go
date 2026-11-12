package api_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Arjunx156/ledgerline/internal/trace"
)

var update = flag.Bool("update", false, "rewrite the recorded traces in testdata/traces")

const tracesDir = "testdata/traces"

// Each scenario is a short story told over the API. Its recorded trace is
// what the proof console is built and tested against (amendment 3.13), so
// a change in what the backend emits shows up here as a diff to review.
var scenarios = []struct {
	name string
	run  func(t *testing.T, s *stack)
}{
	{"one_payment", func(t *testing.T, s *stack) {
		id := s.createIntent(t, nil)
		s.ok(t, "POST", "/v1/payment_intents/"+id+"/confirm", map[string]any{"payment_method": "pm_card_ok"})
	}},
	{"declined", func(t *testing.T, s *stack) {
		id := s.createIntent(t, nil)
		r := s.do(t, "POST", "/v1/payment_intents/"+id+"/confirm", map[string]any{"payment_method": "pm_card_declined"})
		require.Equal(t, http.StatusPaymentRequired, r.status)
	}},
	{"manual_capture", func(t *testing.T, s *stack) {
		id := s.createIntent(t, map[string]any{"capture_method": "manual"})
		s.ok(t, "POST", "/v1/payment_intents/"+id+"/confirm", map[string]any{"payment_method": "pm_card_ok"})
		s.ok(t, "POST", "/v1/payment_intents/"+id+"/capture", map[string]any{"amount_to_capture": 30000})
	}},
	{"processor_timeout", func(t *testing.T, s *stack) {
		id := s.createIntent(t, nil)
		r := s.do(t, "POST", "/v1/payment_intents/"+id+"/confirm", map[string]any{"payment_method": "pm_card_timeout"})
		require.Equal(t, http.StatusServiceUnavailable, r.status)
		s.ok(t, "GET", "/v1/payment_intents/"+id, nil)
	}},
}

func TestRecordedTraces(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			rec := &trace.Memory{}
			sc.run(t, newStack(t, rec, nil))
			got := normalize(t, rec.Events())

			path := filepath.Join(tracesDir, sc.name+".json")
			if *update {
				require.NoError(t, os.MkdirAll(tracesDir, 0o755))
				require.NoError(t, os.WriteFile(path, got, 0o644))
				return
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err, "record it with: go test ./internal/api -run TestRecordedTraces -update")
			assert.Equal(t, string(want), string(got),
				"the trace changed; if that is intended, re-record with -update and review the diff")
		})
	}
}

var (
	idPattern      = regexp.MustCompile(`\b(acct|cus|pi|re|evt|txn|we|whd|req|auth)_[A-Za-z0-9]{24}\b`)
	createdPattern = regexp.MustCompile(`"created": \d+`)
)

// normalize makes a run repeatable: ids become pi_1, pi_2 and so on in
// order of appearance, and wall-clock times become zero.
func normalize(t *testing.T, events []trace.Event) []byte {
	t.Helper()
	for i := range events {
		events[i].At = 0
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(events))

	seen, next := map[string]string{}, map[string]int{}
	out := idPattern.ReplaceAllStringFunc(buf.String(), func(id string) string {
		if n, ok := seen[id]; ok {
			return n
		}
		prefix, _, _ := strings.Cut(id, "_")
		next[prefix]++
		seen[id] = fmt.Sprintf("%s_%d", prefix, next[prefix])
		return seen[id]
	})
	return []byte(createdPattern.ReplaceAllString(out, `"created": 0`))
}

var (
	lanes     = map[trace.Lane]bool{"client": true, "api": true, "postgres": true, "processor": true, "worker": true, "receiver": true}
	siteShape = regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)+$`)
)

// Every recorded trace still parses as the frozen event shape, so the
// console can rely on it.
func TestRecordedTracesKeepTheContract(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(tracesDir, "*.json"))
	require.NoError(t, err)
	require.Len(t, files, len(scenarios), "a trace file without a scenario, or the reverse")
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			require.NoError(t, err)
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			var events []trace.Event
			require.NoError(t, dec.Decode(&events))
			require.NotEmpty(t, events)
			for i, e := range events {
				assert.EqualValues(t, i+1, e.Seq, "seq counts up from 1")
				assert.True(t, lanes[e.Lane], "unknown lane %q", e.Lane)
				assert.Regexp(t, siteShape, e.Site)
				if e.Lane != trace.LaneClient {
					assert.Regexp(t, `^req_\d+$`, e.RequestID, "every server event belongs to a request")
				}
			}
		})
	}
}
