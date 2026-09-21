// Package repositorytest is the conformance suite for control-plane repository
// implementations: one set of behavioral tests the SQLite and Postgres
// repositories must both pass, so the two cannot drift apart.
//
// It exists now, while both implementations are live, on purpose. The
// Postgres-only collapse replaces SQLite's test-double role with an in-memory
// fake, and a suite written against a single implementation proves nothing —
// running it against two independent backends first is what makes it meaningful
// before the fake inherits it.
package repositorytest

import (
	"context"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/control"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Repository is the full control-plane persistence surface the suite exercises.
type Repository interface {
	control.AreasRepository
	control.ProvidersRepository
}

// Factory builds a fresh, empty repository. Implementations register their own
// cleanup on tb.
//
// Unlike storetest, this suite needs genuine per-subtest isolation: areas carry
// store-assigned IDs and List/ListProviders are whole-table reads, so subtests
// cannot partition a shared backend by key the way estimate tests can. Every
// factory must hand back an empty repository.
type Factory func(tb testing.TB) Repository

// carProfile and drivingMode are the profile name every provider fixture maps
// and the OSRM profile it maps to.
const (
	carProfile  = "car"
	drivingMode = "driving"
)

// polygon is a syntactically valid GeoJSON polygon. The repositories store
// geometry as opaque bytes — polyfilling and validation happen above them — so
// its shape only has to survive a round trip.
const polygon = `{"type":"Polygon","coordinates":[[[-97.75,30.26],[-97.74,30.28],[-97.73,30.27],[-97.75,30.26]]]}`

// area builds a candidate area with every field set to something distinctive, so
// a field dropped on the way in or out shows up as a mismatch rather than a
// coincidentally-correct zero.
func area(name string) beeline.Area {
	return beeline.Area{
		Name:            name,
		WarmStrategy:    beeline.WarmHybrid,
		RoutingProvider: "haversine",
		GeoJSON:         []byte(polygon),
		Layers: []beeline.Layer{
			{Resolution: 9, MinDistanceMeters: 0, MaxRadiusMeters: 4000, CoreRadiusMeters: 1500},
			{Resolution: 8, MinDistanceMeters: 4000, MaxRadiusMeters: 12000, CoreRadiusMeters: 6000},
		},
		DemandIdleTTL: 90 * time.Minute,
		TargetTTL:     7 * time.Minute,
		LeaseDuration: 45 * time.Second,
		SweepInterval: 3 * time.Minute,
	}
}

// assertAreaEquals compares every persisted field, ignoring the store-assigned
// id and timestamps.
func assertAreaEquals(t *testing.T, want, got *beeline.Area) {
	t.Helper()

	assert.Equal(t, want.Name, got.Name)
	assert.Equal(t, want.WarmStrategy, got.WarmStrategy)
	assert.Equal(t, want.RoutingProvider, got.RoutingProvider)
	assert.JSONEq(t, string(want.GeoJSON), string(got.GeoJSON))
	assert.Equal(t, want.DemandIdleTTL, got.DemandIdleTTL)
	assert.Equal(t, want.TargetTTL, got.TargetTTL)
	assert.Equal(t, want.LeaseDuration, got.LeaseDuration)
	assert.Equal(t, want.SweepInterval, got.SweepInterval)
	assert.Equal(t, want.Layers, got.Layers, "layers round-trip in order, with every bound")
}

// Run exercises the shared control-plane repository contract against the
// factory's implementation.
func Run(t *testing.T, factory Factory) {
	t.Helper()

	ctx := context.Background()

	t.Run("create assigns an id and round trips every field", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		want := area("downtown")
		created, err := repo.Create(ctx, &want)
		require.NoError(t, err)
		assert.NotZero(t, created.ID, "the store assigns the id")
		assertAreaEquals(t, &want, &created)

		got, err := repo.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, created.ID, got.ID)
		assertAreaEquals(t, &want, &got)
	})

	t.Run("create leaves areas disabled", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		// The control plane's contract: a new area never enters the working set
		// until an operator enables it, so a fresh database boots refreshing
		// nothing.
		want := area("starts-disabled")
		created, err := repo.Create(ctx, &want)
		require.NoError(t, err)
		assert.False(t, created.Enabled, "a created area is disabled")

		got, err := repo.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.False(t, got.Enabled, "and stays disabled when read back")
	})

	t.Run("get on a missing id reports not found", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		_, err := repo.Get(ctx, 424242)
		require.Error(t, err)
		assert.ErrorIs(t, err, beeline.ErrNotFound,
			"every repository's not-found error must satisfy errors.Is(err, beeline.ErrNotFound) — "+
				"the HTTP layer's 404 mapping depends on it")
	})

	t.Run("layers are returned finest first", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		// Load-bearing, not cosmetic: Area.Finest is Layers[0], and it decides
		// area containment and which layer the read path keys at. Insert
		// coarsest-first so a store that simply echoes insertion order fails.
		want := area("ordered")
		want.Layers = []beeline.Layer{
			{Resolution: 7, MinDistanceMeters: 12000, MaxRadiusMeters: 40000, CoreRadiusMeters: 20000},
			{Resolution: 9, MinDistanceMeters: 0, MaxRadiusMeters: 4000, CoreRadiusMeters: 1500},
			{Resolution: 8, MinDistanceMeters: 4000, MaxRadiusMeters: 12000, CoreRadiusMeters: 6000},
		}
		created, err := repo.Create(ctx, &want)
		require.NoError(t, err)

		got, err := repo.Get(ctx, created.ID)
		require.NoError(t, err)
		require.Len(t, got.Layers, 3)
		assert.Equal(t, 9, got.Layers[0].Resolution, "finest layer first")
		assert.Equal(t, 8, got.Layers[1].Resolution)
		assert.Equal(t, 7, got.Layers[2].Resolution, "coarsest layer last")
		assert.Equal(t, 9, got.Finest().Resolution, "Area.Finest agrees")
	})

	t.Run("list returns every area ordered by id", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		listed, err := repo.List(ctx)
		require.NoError(t, err)
		assert.Empty(t, listed, "a fresh repository has no areas")

		ids := make([]beeline.AreaID, 0, 3)
		for _, name := range []string{"first", "second", "third"} {
			candidate := area(name)
			created, createErr := repo.Create(ctx, &candidate)
			require.NoError(t, createErr)
			ids = append(ids, created.ID)
		}

		listed, err = repo.List(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 3)

		gotIDs := make([]beeline.AreaID, 0, len(listed))
		gotNames := make([]string, 0, len(listed))
		for i := range listed {
			gotIDs = append(gotIDs, listed[i].ID)
			gotNames = append(gotNames, listed[i].Name)
		}
		assert.Equal(t, ids, gotIDs, "List is ordered by id, not by insertion coincidence")
		assert.Equal(t, []string{"first", "second", "third"}, gotNames)
		assert.Len(t, listed[0].Layers, 2, "listed areas carry their layers hydrated")
	})

	t.Run("update replaces fields and layers wholesale", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		candidate := area("before")
		created, err := repo.Create(ctx, &candidate)
		require.NoError(t, err)

		updated := created
		updated.Name = "after"
		updated.WarmStrategy = beeline.WarmLazy
		updated.RoutingProvider = "latent-haversine"
		updated.DemandIdleTTL = 5 * time.Minute
		updated.TargetTTL = 11 * time.Minute
		updated.LeaseDuration = 20 * time.Second
		updated.SweepInterval = 90 * time.Second
		// Fewer layers than before, so a store that appends rather than replaces
		// is caught by the count.
		updated.Layers = []beeline.Layer{
			{Resolution: 10, MinDistanceMeters: 0, MaxRadiusMeters: 2000, CoreRadiusMeters: 800},
		}
		require.NoError(t, repo.Update(ctx, &updated))

		got, err := repo.Get(ctx, created.ID)
		require.NoError(t, err)
		assertAreaEquals(t, &updated, &got)
		require.Len(t, got.Layers, 1, "Update replaces the layer set rather than adding to it")
	})

	t.Run("set enabled toggles independently of update", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		candidate := area("toggled")
		created, err := repo.Create(ctx, &candidate)
		require.NoError(t, err)

		require.NoError(t, repo.SetEnabled(ctx, created.ID, true))
		got, err := repo.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.True(t, got.Enabled)
		assert.Equal(t, "toggled", got.Name, "enabling does not disturb other fields")

		// An Update carrying a stale Enabled must not silently re-disable the
		// area: enablement is SetEnabled's business alone.
		stale := got
		stale.Name = "toggled-renamed"
		stale.Enabled = false
		require.NoError(t, repo.Update(ctx, &stale))
		got, err = repo.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, "toggled-renamed", got.Name, "Update applied")
		assert.True(t, got.Enabled, "Update does not own the enabled flag")

		require.NoError(t, repo.SetEnabled(ctx, created.ID, false))
		got, err = repo.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.False(t, got.Enabled)
	})

	t.Run("delete removes the area and cascades to layers", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		doomed := area("doomed")
		created, err := repo.Create(ctx, &doomed)
		require.NoError(t, err)
		survivor := area("survivor")
		kept, err := repo.Create(ctx, &survivor)
		require.NoError(t, err)

		require.NoError(t, repo.Delete(ctx, created.ID))

		_, err = repo.Get(ctx, created.ID)
		assert.ErrorIs(t, err, beeline.ErrNotFound, "the deleted area is gone")

		_, err = repo.Get(ctx, kept.ID)
		require.NoError(t, err, "other areas survive")

		// A layer row orphaned by a non-cascading delete would resurface if the
		// id were reused, so re-create and confirm the new area sees only its own
		// layers.
		replacement := area("replacement")
		replacement.Layers = []beeline.Layer{{Resolution: 6, MaxRadiusMeters: 80000, CoreRadiusMeters: 40000}}
		fresh, err := repo.Create(ctx, &replacement)
		require.NoError(t, err)
		got, err := repo.Get(ctx, fresh.ID)
		require.NoError(t, err)
		assert.Len(t, got.Layers, 1, "no orphaned layer rows survive the delete")
	})

	t.Run("stored areas do not alias caller memory", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		// A persistent store serializes through a database, so it physically
		// cannot share memory with its caller. An in-memory implementation can,
		// and that is the bug class this pins: mutating what you handed in, or
		// what you read back, must not reach stored state.
		candidate := area("no-aliasing")
		created, err := repo.Create(ctx, &candidate)
		require.NoError(t, err)

		// Mutate the input after Create.
		candidate.Name = "mutated-input"
		candidate.Layers[0].Resolution = 99
		if len(candidate.GeoJSON) > 0 {
			candidate.GeoJSON[0] = 'X'
		}

		// Mutate what Create returned. Only the reference fields are meaningful
		// here — a returned struct's scalars are a copy by construction and
		// cannot alias anything.
		created.Layers[0].MaxRadiusMeters = -1

		got, err := repo.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, "no-aliasing", got.Name, "stored name is untouched by caller mutations")
		assert.Equal(t, 9, got.Layers[0].Resolution, "stored layers are untouched")
		assert.InDelta(t, 4000, got.Layers[0].MaxRadiusMeters, 1e-9)
		assert.JSONEq(t, polygon, string(got.GeoJSON), "stored geometry is untouched")

		// And mutating one read must not affect the next.
		got.Layers[0].Resolution = 1
		again, err := repo.Get(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, 9, again.Layers[0].Resolution, "each read is independent")
	})

	t.Run("update on a missing id fails and creates nothing", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		// The UPDATE itself matches no rows, but Update also replaces the layer
		// set, and layer rows for a nonexistent area have nothing to hang off —
		// both SQL stores surface a foreign-key violation. What matters for a
		// fake is only that it refuses too: silently accepting a write the real
		// stores reject is precisely how a double starts lying.
		//
		// The error *kind* is deliberately unpinned. control.Update calls Get
		// first, so nothing in production reaches this path, and requiring
		// ErrNotFound here would mean adding an existence check to both stores
		// for a case no caller can hit.
		ghost := area("ghost")
		ghost.ID = 999111
		require.Error(t, repo.Update(ctx, &ghost), "updating an area that does not exist must fail")

		_, err := repo.Get(ctx, ghost.ID)
		assert.ErrorIs(t, err, beeline.ErrNotFound, "and it did not conjure the area into existence")
	})

	t.Run("stored providers do not alias caller memory", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		spec := beeline.ProviderSpec{
			Name:     "aliasing",
			Type:     beeline.ProviderTypeOSRM,
			BaseURL:  "http://one.invalid",
			Profiles: map[string]string{carProfile: drivingMode},
		}
		require.NoError(t, repo.UpsertProvider(ctx, &spec))

		// The profile map is the reference field a fake is most likely to share.
		spec.Profiles[carProfile] = "mutated"
		spec.Profiles["added"] = "later"

		listed, err := repo.ListProviders(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, map[string]string{carProfile: drivingMode}, listed[0].Profiles,
			"the stored profile map is independent of the caller's")

		// And what ListProviders hands back is independent too.
		listed[0].Profiles[carProfile] = "mutated-again"
		again, err := repo.ListProviders(ctx)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{carProfile: drivingMode}, again[0].Profiles)
	})

	t.Run("provider upsert inserts then updates under one name", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		listed, err := repo.ListProviders(ctx)
		require.NoError(t, err)
		assert.Empty(t, listed,
			"a fresh repository stores no providers — built-ins are synthesized by control, never persisted")

		// Every field populated, so one dropped on the way through shows up.
		spec := beeline.ProviderSpec{
			Name:         "osrm-a",
			Type:         beeline.ProviderTypeOSRM,
			BaseURL:      "http://one.invalid",
			TimeoutMs:    2500,
			MaxTableSize: 4096,
			Profiles:     map[string]string{carProfile: drivingMode, "bike": "cycling"},
		}
		require.NoError(t, repo.UpsertProvider(ctx, &spec))

		listed, err = repo.ListProviders(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, spec, listed[0], "the whole spec round-trips")

		// Same name again: an update, not a second row.
		spec.BaseURL = "http://two.invalid"
		spec.TimeoutMs = 7000
		spec.Profiles = map[string]string{carProfile: drivingMode}
		require.NoError(t, repo.UpsertProvider(ctx, &spec))

		listed, err = repo.ListProviders(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 1, "upsert under an existing name replaces rather than duplicating")
		assert.Equal(t, spec, listed[0], "and the replacement is complete, not a merge")
	})

	t.Run("list providers is ordered by name", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		// Inserted out of order so an implementation echoing insertion order fails.
		for _, name := range []string{"zulu", "alpha", "mike"} {
			spec := beeline.ProviderSpec{Name: name, Type: beeline.ProviderTypeOSRM, BaseURL: "http://" + name + ".invalid"}
			require.NoError(t, repo.UpsertProvider(ctx, &spec))
		}

		listed, err := repo.ListProviders(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 3)
		names := make([]string, 0, len(listed))
		for i := range listed {
			names = append(names, listed[i].Name)
		}
		assert.Equal(t, []string{"alpha", "mike", "zulu"}, names, "ListProviders is ordered by name")
	})

	t.Run("delete provider removes one and tolerates an absent name", func(t *testing.T) {
		t.Parallel()
		repo := factory(t)

		keep := beeline.ProviderSpec{Name: "keep", Type: beeline.ProviderTypeOSRM, BaseURL: "http://keep.invalid"}
		drop := beeline.ProviderSpec{Name: "drop", Type: beeline.ProviderTypeOSRM, BaseURL: "http://drop.invalid"}
		require.NoError(t, repo.UpsertProvider(ctx, &keep))
		require.NoError(t, repo.UpsertProvider(ctx, &drop))

		require.NoError(t, repo.DeleteProvider(ctx, "drop"))

		listed, err := repo.ListProviders(ctx)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, "keep", listed[0].Name)

		// The repository layer is not where absence is an error: control decides
		// whether a missing provider is a 404, and it needs a plain report back.
		require.NoError(t, repo.DeleteProvider(ctx, "never-existed"),
			"deleting an absent provider is a no-op at the repository layer")
	})
}
