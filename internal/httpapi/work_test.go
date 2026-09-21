package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	"github.com/primandproper/beeline/internal/httpapi"
	memstore "github.com/primandproper/beeline/internal/store/memory"

	"github.com/primandproper/primitives-go/v2/observability/logging"
	metricsnoop "github.com/primandproper/primitives-go/v2/observability/metrics/noop"
	tracingnoop "github.com/primandproper/primitives-go/v2/observability/tracing/noop"
	chibackend "github.com/primandproper/primitives-go/v2/routing/backends/chi"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimEnvelope / submitEnvelope mirror the /_work_/ wire shapes for decoding.
type claimEnvelope struct {
	Areas map[string]struct {
		RoutingProvider string `json:"routingProvider"`
	} `json:"areas"`
	Pairs []struct {
		Profile string `json:"profile"`
		Origin  string `json:"origin"`
		Dest    string `json:"dest"`
		Area    int64  `json:"area"`
		Res     int    `json:"res"`
	} `json:"pairs"`
	LeaseSeconds int `json:"leaseSeconds"`
}

type submitEnvelope struct {
	Accepted int `json:"accepted"`
}

// newWorkRouter wires the /_work_/ endpoints over a real memory index+store with
// no coordinator (nil Coordinator omits provider metadata and disables the
// enabled-area submit filter).
func newWorkRouter(t *testing.T, index beeline.FreshnessIndex, store beeline.Store) http.Handler {
	t.Helper()

	router := httpapi.NewRouter(
		logging.EnsureLogger(nil),
		tracingnoop.NewTracerProvider(),
		metricsnoop.NewMetricsProvider(),
		&chibackend.Config{ServiceName: "test"},
	)
	httpapi.Register(router, &httpapi.Deps{
		Index:          index,
		Store:          store,
		DefaultProfile: "car",
		RefreshBatch:   4,
		LeaseDuration:  30 * time.Second,
	})

	return router.Handler()
}

func postJSON(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

// seedWorkPairs seeds n pairs in area 1 at resolution 9 and returns them.
func seedWorkPairs(t *testing.T, index beeline.FreshnessIndex, n int) []beeline.PairKey {
	t.Helper()

	origin, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 9)
	require.NoError(t, err)

	pairs := make([]beeline.PairKey, 0, n)
	for i := range n {
		dest, destErr := beeline.CellAt(beeline.LatLng{Lat: 37.70 + float64(i)*0.01, Lng: -122.4194}, 9)
		require.NoError(t, destErr)
		pairs = append(pairs, beeline.PairKey{Area: 1, Origin: origin, Dest: dest, Profile: "car", Res: 9})
	}
	require.NoError(t, index.Seed(context.Background(), pairs))

	return pairs
}

func TestWorkClaimHandsOutSeededPairs(t *testing.T) {
	t.Parallel()

	index := memindex.New(time.Minute, nil)
	store := memstore.New()
	h := newWorkRouter(t, index, store)
	seeded := seedWorkPairs(t, index, 3)

	rec := postJSON(t, h, "/_work_/claim", `{"batchSize": 10, "leaseSeconds": 90}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var env claimEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, 90, env.LeaseSeconds, "the requested lease is echoed")
	require.Len(t, env.Pairs, len(seeded))
	for _, p := range env.Pairs {
		assert.Equal(t, int64(1), p.Area)
		assert.Equal(t, "car", p.Profile)
		assert.Equal(t, 9, p.Res)
		assert.Equal(t, seeded[0].Origin.String(), p.Origin, "all pairs share the seeded origin")
	}
	assert.Empty(t, env.Areas, "no coordinator, no provider metadata")

	// The pairs are leased now: a second claim comes back empty.
	rec = postJSON(t, h, "/_work_/claim", `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Empty(t, env.Pairs)
	assert.Equal(t, 30, env.LeaseSeconds, "zero lease falls back to the leader default")
}

func TestWorkClaimRejectsNegatives(t *testing.T) {
	t.Parallel()

	h := newWorkRouter(t, memindex.New(time.Minute, nil), memstore.New())

	rec := postJSON(t, h, "/_work_/claim", `{"batchSize": -1}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = postJSON(t, h, "/_work_/claim", `{"leaseSeconds": -5}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestWorkSubmitWritesAndBurnsDebt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	index := memindex.New(time.Minute, nil)
	store := memstore.New()
	h := newWorkRouter(t, index, store)
	seeded := seedWorkPairs(t, index, 2)

	rec := postJSON(t, h, "/_work_/claim", `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var claim claimEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &claim))
	require.Len(t, claim.Pairs, 2)

	results := make([]map[string]any, 0, len(claim.Pairs))
	for _, p := range claim.Pairs {
		results = append(results, map[string]any{
			"profile": p.Profile, "origin": p.Origin, "dest": p.Dest,
			"area": p.Area, "res": p.Res,
			"durationSec": 123.0, "distanceMeters": 456.0,
		})
	}
	body, err := json.Marshal(map[string]any{"results": results})
	require.NoError(t, err)

	rec = postJSON(t, h, "/_work_/submit", string(body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var sub submitEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sub))
	assert.Equal(t, 2, sub.Accepted)

	stored, err := store.BatchGet(ctx, seeded)
	require.NoError(t, err)
	for _, s := range stored {
		require.NotNil(t, s)
		assert.Equal(t, 123.0, s.Duration)
		assert.Equal(t, 456.0, s.Distance)
		assert.False(t, s.ComputedAt.IsZero(), "the leader stamped ComputedAt")
	}

	debt, err := index.Debt(ctx)
	require.NoError(t, err)
	assert.Zero(t, debt.Debt, "submit marked the pairs computed")

	// A duplicate submit is wasteful but harmless (idempotent Put/MarkComputed).
	rec = postJSON(t, h, "/_work_/submit", string(body))
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestWorkSubmitRejectsBadBodies(t *testing.T) {
	t.Parallel()

	h := newWorkRouter(t, memindex.New(time.Minute, nil), memstore.New())

	rec := postJSON(t, h, "/_work_/submit", `{"results": []}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "empty results")

	rec = postJSON(t, h, "/_work_/submit",
		`{"results": [{"profile":"car","origin":"nonsense","dest":"892830828b3ffff","area":1,"res":9}]}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "invalid origin cell")

	// One result past the cap.
	var results bytes.Buffer
	results.WriteString(`{"results": [`)
	for i := range 10_001 {
		if i > 0 {
			results.WriteString(",")
		}
		fmt.Fprintf(&results, `{"profile":"car","origin":"892830828abffff","dest":"892830828b3ffff","area":1,"res":9,"durationSec":1,"distanceMeters":1}`)
	}
	results.WriteString(`]}`)
	rec = postJSON(t, h, "/_work_/submit", results.String())
	assert.Equal(t, http.StatusBadRequest, rec.Code, "over the result cap")
}
