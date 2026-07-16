package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Environment pairs a named, real Config object with the path its rendered JSON
// should be written to. It is the unit Render operates on: the Config is built
// in Go (see cmd/tools/codegen/configs), so the checked-in JSON files are always
// a faithful, type-checked projection of that source of truth rather than
// hand-maintained text.
type Environment struct {
	// Config is the real configuration object for this environment.
	Config *Config
	// Name labels the environment in log and error messages (e.g. "localdev").
	Name string
	// Path is where the rendered JSON is written, relative to the working
	// directory. Parent directories are created as needed.
	Path string
}

// Render writes each environment's Config to its Path as pretty-printed JSON.
// When validate is true every Config is validated before anything is written, so
// an invalid config fails the whole run instead of landing a broken file on
// disk. The rendered files round-trip through LoadFromFile.
func Render(ctx context.Context, envs []Environment, validate bool) error {
	if validate {
		for _, env := range envs {
			if err := env.Config.Validate(ctx); err != nil {
				return fmt.Errorf("validating %s config: %w", env.Name, err)
			}
		}
	}

	for _, env := range envs {
		data, err := json.MarshalIndent(env.Config, "", "\t")
		if err != nil {
			return fmt.Errorf("marshaling %s config: %w", env.Name, err)
		}
		data = append(data, '\n')

		if err = os.MkdirAll(filepath.Dir(env.Path), 0o750); err != nil {
			return fmt.Errorf("creating directory for %s config: %w", env.Name, err)
		}

		if err = os.WriteFile(env.Path, data, 0o600); err != nil {
			return fmt.Errorf("writing %s config: %w", env.Name, err)
		}
	}

	return nil
}
