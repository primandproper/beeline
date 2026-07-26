package sqlite

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/store/sqlite/generated"
)

// ListProviders returns every operator-defined provider spec, ordered by name.
// Built-in providers are synthesized by the control plane, never stored here.
func (r *Repository) ListProviders(ctx context.Context) ([]beeline.ProviderSpec, error) {
	rows, err := r.queries.ListProviders(ctx, r.db)
	if err != nil {
		return nil, fmt.Errorf("sqlite: listing providers: %w", err)
	}

	specs := make([]beeline.ProviderSpec, 0, len(rows))
	for _, row := range rows {
		spec, convErr := convertProvider(row)
		if convErr != nil {
			return nil, convErr
		}
		specs = append(specs, spec)
	}

	return specs, nil
}

// UpsertProvider inserts or replaces one provider spec by name, stamping
// created/updated times (created_at survives an update via the upsert).
func (r *Repository) UpsertProvider(ctx context.Context, spec *beeline.ProviderSpec) error {
	profiles, err := json.Marshal(profilesOrEmpty(spec.Profiles))
	if err != nil {
		return fmt.Errorf("sqlite: encoding profiles for provider %q: %w", spec.Name, err)
	}

	now := r.clock.Now().UTC().Format(timeFormat)
	if err = r.queries.UpsertProvider(ctx, r.db, &generated.UpsertProviderParams{
		Name:         spec.Name,
		Type:         spec.Type,
		BaseUrl:      spec.BaseURL,
		ProfilesJson: string(profiles),
		MaxTableSize: int64(spec.MaxTableSize),
		TimeoutMs:    spec.TimeoutMs,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		return fmt.Errorf("sqlite: upserting provider %q: %w", spec.Name, err)
	}

	return nil
}

// DeleteProvider removes one provider by name. Deleting an absent name is a no-op;
// referential safety (no area may still name it) is the control plane's job.
func (r *Repository) DeleteProvider(ctx context.Context, name string) error {
	if err := r.queries.DeleteProvider(ctx, r.db, name); err != nil {
		return fmt.Errorf("sqlite: deleting provider %q: %w", name, err)
	}

	return nil
}

// convertProvider maps a generated row to the domain spec.
func convertProvider(row *generated.Providers) (beeline.ProviderSpec, error) {
	var profiles map[string]string
	if err := json.Unmarshal([]byte(row.ProfilesJson), &profiles); err != nil {
		return beeline.ProviderSpec{}, fmt.Errorf("sqlite: decoding profiles for provider %q: %w", row.Name, err)
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

// profilesOrEmpty keeps the stored JSON a map (never null) so decoding stays uniform.
func profilesOrEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}

	return m
}
