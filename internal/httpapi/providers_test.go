package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// providerEnvelope mirrors the /_config_/providers wire shape.
type providerEnvelope struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"baseURL"`
	Builtin bool   `json:"builtin"`
}

// catalogEnvelope mirrors the /_work_/providers wire shape.
type catalogEnvelope struct {
	Speeds    map[string]float64 `json:"profiles"`
	Hash      string             `json:"hash"`
	Providers []providerEnvelope `json:"providers"`
}

func doJSON(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), method, path, bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func TestProviderEndpointsLifecycle(t *testing.T) {
	t.Parallel()

	h := newWarmHarness(t).handler

	rec := doJSON(t, h, http.MethodGet, "/_config_/providers", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listed []providerEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	require.NotEmpty(t, listed)
	assert.Equal(t, config.DefaultProviderName, listed[0].Name)
	assert.True(t, listed[0].Builtin)

	// The catalog is served with a hash, and the claim response carries the same hash.
	rec = doJSON(t, h, http.MethodGet, "/_work_/providers", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var catalog catalogEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &catalog))
	require.NotEmpty(t, catalog.Hash)
	assert.Equal(t, map[string]float64{"car": 10}, catalog.Speeds)

	rec = doJSON(t, h, http.MethodPost, "/_work_/claim", `{"batchSize": 1}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var claim struct {
		ProvidersHash string `json:"providersHash"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &claim))
	assert.Equal(t, catalog.Hash, claim.ProvidersHash)

	// Upserting a provider changes the hash and shows up in the catalog.
	rec = doJSON(t, h, http.MethodPut, "/_config_/providers/osrm-west",
		`{"type": "osrm", "baseURL": "http://osrm-west:5000", "timeoutMs": 2000}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var put providerEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &put))
	assert.Equal(t, "osrm-west", put.Name, "the path names the provider")
	assert.False(t, put.Builtin)

	rec = doJSON(t, h, http.MethodGet, "/_work_/providers", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var after catalogEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &after))
	assert.NotEqual(t, catalog.Hash, after.Hash)
	names := make([]string, 0, len(after.Providers))
	for _, p := range after.Providers {
		names = append(names, p.Name)
	}
	assert.Contains(t, names, "osrm-west")

	// Delete works once nothing references it; deleting again is a 404.
	rec = doJSON(t, h, http.MethodDelete, "/_config_/providers/osrm-west", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = doJSON(t, h, http.MethodDelete, "/_config_/providers/osrm-west", "")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

func TestProviderEndpointsRejections(t *testing.T) {
	t.Parallel()

	h := newWarmHarness(t).handler

	t.Run("built-in names are reserved", func(t *testing.T) {
		t.Parallel()

		rec := doJSON(t, h, http.MethodPut, "/_config_/providers/"+beeline.DefaultProviderName,
			`{"type": "haversine"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

		rec = doJSON(t, h, http.MethodDelete, "/_config_/providers/"+beeline.DefaultProviderName, "")
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("osrm without a baseURL is invalid", func(t *testing.T) {
		t.Parallel()

		rec := doJSON(t, h, http.MethodPut, "/_config_/providers/broken", `{"type": "osrm"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("latent-haversine cannot be operator-created", func(t *testing.T) {
		t.Parallel()

		rec := doJSON(t, h, http.MethodPut, "/_config_/providers/sneaky", `{"type": "latent-haversine"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("delete is a conflict while an area references the provider", func(t *testing.T) {
		t.Parallel()

		rec := doJSON(t, h, http.MethodPut, "/_config_/providers/osrm-clung",
			`{"type": "osrm", "baseURL": "http://osrm-clung:5000"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		geo := `{"type":"Polygon","coordinates":[[[-122.45,37.75],[-122.39,37.75],[-122.39,37.80],[-122.45,37.80],[-122.45,37.75]]]}`
		rec = doJSON(t, h, http.MethodPost, "/_config_/areas",
			`{"name": "clinger", "routingProvider": "osrm-clung", "geojson": `+geo+
				`, "layers": [{"resolution": 8, "maxRadiusMeters": 5000}]}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		rec = doJSON(t, h, http.MethodDelete, "/_config_/providers/osrm-clung", "")
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	})
}
