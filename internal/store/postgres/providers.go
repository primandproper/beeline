package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/store/postgres/generated"

	"github.com/jackc/pgx/v5"
)

// ListProviders returns every operator-defined provider spec, ordered by name.
// Built-ins are synthesized by the control plane, never stored.
func (r *Repository) ListProviders(ctx context.Context) ([]beeline.ProviderSpec, error) {
	rows, err := r.queries.ListProviders(ctx, r.pool)
	if err != nil {
		return nil, fmt.Errorf("postgres: listing providers: %w", err)
	}

	specs := make([]beeline.ProviderSpec, 0, len(rows))
	for _, row := range rows {
		spec, convErr := convertProviderRow(row)
		if convErr != nil {
			return nil, convErr
		}
		specs = append(specs, spec)
	}

	return specs, nil
}

// UpsertProvider inserts or replaces one provider spec by name (created_at
// survives an update via the upsert) and bumps the providers generation, so
// every head rebuilds its engines and catalog hash on its next poll.
func (r *Repository) UpsertProvider(ctx context.Context, spec *beeline.ProviderSpec) error {
	profiles, err := json.Marshal(profilesOrEmpty(spec.Profiles))
	if err != nil {
		return fmt.Errorf("postgres: encoding profiles for provider %q: %w", spec.Name, err)
	}

	now := timestamptz(r.now().UTC())

	return r.inTx(ctx, func(tx pgx.Tx) error {
		if upErr := r.queries.UpsertProvider(ctx, tx, &generated.UpsertProviderParams{
			Name:         spec.Name,
			Type:         spec.Type,
			BaseUrl:      spec.BaseURL,
			ProfilesJson: string(profiles),
			MaxTableSize: int64(spec.MaxTableSize),
			TimeoutMs:    spec.TimeoutMs,
			CreatedAt:    now,
			UpdatedAt:    now,
		}); upErr != nil {
			return fmt.Errorf("postgres: upserting provider %q: %w", spec.Name, upErr)
		}

		return r.bump(ctx, tx, ConfigKindProviders)
	})
}

// DeleteProvider removes one provider by name and bumps the providers
// generation. Deleting an absent name is a no-op; referential safety is the
// control plane's job.
func (r *Repository) DeleteProvider(ctx context.Context, name string) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		if err := r.queries.DeleteProvider(ctx, tx, name); err != nil {
			return fmt.Errorf("postgres: deleting provider %q: %w", name, err)
		}

		return r.bump(ctx, tx, ConfigKindProviders)
	})
}

// convertProviderRow maps a generated row to the domain spec.
func convertProviderRow(row *generated.Providers) (beeline.ProviderSpec, error) {
	var profiles map[string]string
	if err := json.Unmarshal([]byte(row.ProfilesJson), &profiles); err != nil {
		return beeline.ProviderSpec{}, fmt.Errorf("postgres: decoding profiles for provider %q: %w", row.Name, err)
	}
	if len(profiles) == 0 {
		profiles = nil
	}

	return beeline.ProviderSpec{
		Name:         row.Name,
		Type:         row.Type,
		BaseURL:      row.BaseUrl,
		Profiles:     profiles,
		MaxTableSize: int(row.MaxTableSize),
		TimeoutMs:    row.TimeoutMs,
	}, nil
}

// profilesOrEmpty keeps the stored JSON a map (never null) so decoding stays
// uniform.
func profilesOrEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}

	return m
}
