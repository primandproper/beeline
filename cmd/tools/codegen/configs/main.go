// Command configs renders the application's per-environment configuration files
// from real, typed Go objects. It is the source of truth behind `make configs`:
// each environment's Config is built in Go (see environments.go), validated, and
// written to disk as JSON. Regenerate whenever a builder or the Config struct
// changes, and commit the result so the checked-in JSON never drifts from the
// code.
package main

import (
	"context"
	"log"

	"github.com/primandproper/template-go/internal/config"
)

func main() {
	envs := []config.Environment{
		{
			Name:   "localdev",
			Path:   "config/localdev.json",
			Config: buildLocalDevConfig(),
		},
		{
			Name:   "production",
			Path:   "config/production.json",
			Config: buildProductionConfig(),
		},
	}

	if err := config.Render(context.Background(), envs, true); err != nil {
		log.Fatalf("rendering configs: %v", err)
	}

	log.Printf("rendered %d config file(s)", len(envs))
}
