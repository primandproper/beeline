package osrm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/engine/osrm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTable(t *testing.T) {
	t.Parallel()

	sf := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	oak := beeline.LatLng{Lat: 37.8044, Lng: -122.2712}
	brk := beeline.LatLng{Lat: 37.8715, Lng: -122.2730}

	t.Run("builds a 1x2 request and parses duration and distance", func(t *testing.T) {
		t.Parallel()

		var gotPath, gotSources, gotDests, gotAnnotations string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotSources = r.URL.Query().Get("sources")
			gotDests = r.URL.Query().Get("destinations")
			gotAnnotations = r.URL.Query().Get("annotations")
			_, _ = w.Write([]byte(`{"code":"Ok","durations":[[600,900]],"distances":[[8000,12000]]}`))
		}))
		defer srv.Close()

		eng := osrm.New(srv.URL, osrm.WithProfiles(map[beeline.Profile]string{"car": "driving"}))
		resp, err := eng.Table(context.Background(), beeline.TableRequest{
			Profile:      "car",
			Sources:      []beeline.LatLng{sf},
			Destinations: []beeline.LatLng{oak, brk},
			Want:         beeline.AnnotateDuration | beeline.AnnotateDistance,
		})
		require.NoError(t, err)

		// One source, two destinations packed into a single coordinate list.
		assert.Equal(t, "/table/v1/driving/-122.419400,37.774900;-122.271200,37.804400;-122.273000,37.871500", gotPath)
		assert.Equal(t, "0", gotSources)
		assert.Equal(t, "1;2", gotDests)
		assert.Equal(t, "duration,distance", gotAnnotations)

		require.Len(t, resp.Duration, 1)
		assert.Equal(t, []float64{600, 900}, resp.Duration[0])
		require.Len(t, resp.Distance, 1)
		assert.Equal(t, []float64{8000, 12000}, resp.Distance[0])
	})

	t.Run("duration-only request omits distance", func(t *testing.T) {
		t.Parallel()

		var gotAnnotations string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAnnotations = r.URL.Query().Get("annotations")
			_, _ = w.Write([]byte(`{"code":"Ok","durations":[[600]]}`))
		}))
		defer srv.Close()

		eng := osrm.New(srv.URL)
		resp, err := eng.Table(context.Background(), beeline.TableRequest{
			Profile:      "car",
			Sources:      []beeline.LatLng{sf},
			Destinations: []beeline.LatLng{oak},
			Want:         beeline.AnnotateDuration,
		})
		require.NoError(t, err)

		assert.Equal(t, "duration", gotAnnotations)
		assert.Equal(t, [][]float64{{600}}, resp.Duration)
		assert.Nil(t, resp.Distance)
	})

	t.Run("an unreachable cell (null) maps to zero", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"code":"Ok","durations":[[600,null]]}`))
		}))
		defer srv.Close()

		eng := osrm.New(srv.URL)
		resp, err := eng.Table(context.Background(), beeline.TableRequest{
			Profile:      "car",
			Sources:      []beeline.LatLng{sf},
			Destinations: []beeline.LatLng{oak, brk},
		})
		require.NoError(t, err)
		assert.Equal(t, [][]float64{{600, 0}}, resp.Duration)
	})

	t.Run("a non-Ok code is an error", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"code":"NoSegment","message":"could not snap"}`))
		}))
		defer srv.Close()

		eng := osrm.New(srv.URL)
		_, err := eng.Table(context.Background(), beeline.TableRequest{
			Profile:      "car",
			Sources:      []beeline.LatLng{sf},
			Destinations: []beeline.LatLng{oak},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "NoSegment")
	})

	t.Run("a non-200 status is an error", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("upstream down"))
		}))
		defer srv.Close()

		eng := osrm.New(srv.URL)
		_, err := eng.Table(context.Background(), beeline.TableRequest{
			Profile:      "car",
			Sources:      []beeline.LatLng{sf},
			Destinations: []beeline.LatLng{oak},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "503")
	})

	t.Run("table size beyond the configured max is rejected before any request", func(t *testing.T) {
		t.Parallel()

		var called bool
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			called = true
		}))
		defer srv.Close()

		eng := osrm.New(srv.URL, osrm.WithMaxTableSize(1))
		_, err := eng.Table(context.Background(), beeline.TableRequest{
			Profile:      "car",
			Sources:      []beeline.LatLng{sf},
			Destinations: []beeline.LatLng{oak, brk},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds max")
		assert.False(t, called, "no request should be issued once the bound is exceeded")
	})

	t.Run("empty sources or destinations returns an empty response without a request", func(t *testing.T) {
		t.Parallel()

		var called bool
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			called = true
		}))
		defer srv.Close()

		eng := osrm.New(srv.URL)
		resp, err := eng.Table(context.Background(), beeline.TableRequest{
			Profile: "car",
			Sources: []beeline.LatLng{sf},
		})
		require.NoError(t, err)
		assert.Nil(t, resp.Duration)
		assert.False(t, called)
	})

	t.Run("a cancelled context aborts the request", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"code":"Ok","durations":[[1]]}`))
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		eng := osrm.New(srv.URL)
		_, err := eng.Table(ctx, beeline.TableRequest{
			Profile:      "car",
			Sources:      []beeline.LatLng{sf},
			Destinations: []beeline.LatLng{oak},
		})
		require.Error(t, err)
	})
}

func TestProfileFallbackAndCapabilities(t *testing.T) {
	t.Parallel()

	t.Run("an unmapped profile passes through unchanged", func(t *testing.T) {
		t.Parallel()

		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			_, _ = w.Write([]byte(`{"code":"Ok","durations":[[1]]}`))
		}))
		defer srv.Close()

		eng := osrm.New(srv.URL, osrm.WithProfiles(map[beeline.Profile]string{"car": "driving"}))
		_, err := eng.Table(context.Background(), beeline.TableRequest{
			Profile:      "bike",
			Sources:      []beeline.LatLng{{Lat: 1, Lng: 2}},
			Destinations: []beeline.LatLng{{Lat: 3, Lng: 4}},
		})
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(gotPath, "/table/v1/bike/"), "unmapped profile should be used verbatim")
	})

	t.Run("capabilities report configured profiles, distance, and max table size", func(t *testing.T) {
		t.Parallel()

		eng := osrm.New("http://osrm:5000",
			osrm.WithProfiles(map[beeline.Profile]string{"car": "driving"}),
			osrm.WithMaxTableSize(10000),
		)
		caps := eng.Capabilities()
		assert.True(t, caps.SupportsDistance)
		assert.Equal(t, 10000, caps.MaxTableSize)
		assert.Contains(t, caps.SupportedProfiles, beeline.Profile("car"))
	})

	t.Run("a trailing slash on the base URL does not double up", func(t *testing.T) {
		t.Parallel()

		var gotURL string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotURL = r.URL.String()
			_, _ = w.Write([]byte(`{"code":"Ok","durations":[[1]]}`))
		}))
		defer srv.Close()

		eng := osrm.New(srv.URL+"/", osrm.WithHTTPClient(srv.Client()))
		_, err := eng.Table(context.Background(), beeline.TableRequest{
			Profile:      "car",
			Sources:      []beeline.LatLng{{Lat: 1, Lng: 2}},
			Destinations: []beeline.LatLng{{Lat: 3, Lng: 4}},
		})
		require.NoError(t, err)

		parsed, perr := url.Parse(gotURL)
		require.NoError(t, perr)
		assert.False(t, strings.Contains(parsed.Path, "//"), "path should not contain a doubled slash")
	})
}
