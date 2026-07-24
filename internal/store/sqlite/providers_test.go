package sqlite_test

import (
	"context"
	"testing"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepositoryProvidersRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	specs, err := repo.ListProviders(ctx)
	require.NoError(t, err)
	assert.Empty(t, specs, "a fresh database has no providers")

	osrm := beeline.ProviderSpec{
		Name:         "osrm-west",
		Type:         beeline.ProviderTypeOSRM,
		BaseURL:      "http://osrm-west.internal:5000",
		Profiles:     map[string]string{"car": "driving"},
		MaxTableSize: 500,
		TimeoutMs:    2500,
	}
	require.NoError(t, repo.UpsertProvider(ctx, &osrm))
	require.NoError(t, repo.UpsertProvider(ctx, &beeline.ProviderSpec{
		Name: "flat-earth",
		Type: beeline.ProviderTypeHaversine,
	}))

	specs, err = repo.ListProviders(ctx)
	require.NoError(t, err)
	require.Len(t, specs, 2)
	assert.Equal(t, "flat-earth", specs[0].Name, "ordered by name")
	assert.Equal(t, osrm, specs[1])
	assert.Nil(t, specs[0].Profiles, "an empty profile map round-trips as nil")
}

func TestRepositoryUpsertProviderReplaces(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	require.NoError(t, repo.UpsertProvider(ctx, &beeline.ProviderSpec{
		Name:    "osrm-west",
		Type:    beeline.ProviderTypeOSRM,
		BaseURL: "http://old.internal:5000",
	}))
	require.NoError(t, repo.UpsertProvider(ctx, &beeline.ProviderSpec{
		Name:      "osrm-west",
		Type:      beeline.ProviderTypeOSRM,
		BaseURL:   "http://new.internal:5000",
		TimeoutMs: 1000,
	}))

	specs, err := repo.ListProviders(ctx)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.Equal(t, "http://new.internal:5000", specs[0].BaseURL)
	assert.Equal(t, int64(1000), specs[0].TimeoutMs)
}

func TestRepositoryDeleteProvider(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	require.NoError(t, repo.UpsertProvider(ctx, &beeline.ProviderSpec{
		Name: "doomed", Type: beeline.ProviderTypeHaversine,
	}))
	require.NoError(t, repo.DeleteProvider(ctx, "doomed"))
	require.NoError(t, repo.DeleteProvider(ctx, "doomed"), "deleting an absent name is a no-op")

	specs, err := repo.ListProviders(ctx)
	require.NoError(t, err)
	assert.Empty(t, specs)
}
