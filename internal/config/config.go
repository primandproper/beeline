// Package config assembles the application's configuration, most notably the
// observability settings that the platform-go observability suite consumes.
//
// The configuration is built in Go with sensible, zero-dependency defaults so
// the binary boots out of the box: structured slog logging plus noop tracing,
// metrics, and profiling. Callers may override the service name and log level
// (see Options), which is how the CLI threads its flags and environment into
// the platform configuration.
//
// Two loaders build on those defaults using platform-go's config package:
//
//   - Load overlays environment variables (prefixed with EnvVarPrefix) on top of
//     the defaults, so any field can be tuned without a config file — the
//     twelve-factor path most deployments start with.
//   - LoadFromFile decodes a complete JSON config file and then overlays the
//     same environment variables, for deployments that graduate to a checked-in
//     or mounted configuration.
//
// Both share envVarOptions, which wires the app's env var prefix and a debug
// hook that logs every value the parser applies.
package config

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	platformconfig "github.com/primandproper/platform-go/v4/config"
	"github.com/primandproper/platform-go/v4/observability"
	"github.com/primandproper/platform-go/v4/observability/logging"
	loggingcfg "github.com/primandproper/platform-go/v4/observability/logging/config"
	metricsnoop "github.com/primandproper/platform-go/v4/observability/metrics/noop"
	profilingnoop "github.com/primandproper/platform-go/v4/observability/profiling/noop"
	tracingnoop "github.com/primandproper/platform-go/v4/observability/tracing/noop"
)

// DefaultServiceName is the service name reported by the observability suite
// when the caller does not supply one.
const DefaultServiceName = "template-go"

// EnvVarPrefix is prepended to every environment variable this application
// reads, keeping its configuration in a distinct namespace. For example the
// logging level is read from TEMPLATE_GO_OBSERVABILITY_LOGGING_LEVEL: the prefix
// here, then the nested envPrefix tags on Config and the platform sub-configs.
const EnvVarPrefix = "TEMPLATE_GO_"

// Log level names accepted by Options.LogLevel (case-insensitive).
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Config is the application configuration. It wraps the platform-go
// observability configuration; add your own fields here as the application grows.
//
// The struct tags let platform-go's config package populate the config from
// environment variables (envPrefix) and JSON files (json). Give new fields both
// tags so they participate in Load and LoadFromFile.
type Config struct {
	Observability observability.Config `envPrefix:"OBSERVABILITY_" json:"observability"`
}

// Options tune the values that most deployments care about. Empty fields fall
// back to the built-in defaults.
type Options struct {
	// ServiceName labels telemetry emitted by the service.
	ServiceName string
	// LogLevel is one of "debug", "info", "warn", or "error" (case-insensitive).
	LogLevel string
}

// New builds a Config from the given options, filling in defaults. The result
// validates cleanly and is ready to hand to observability.Config.NewPillars.
func New(opts Options) *Config {
	serviceName := strings.TrimSpace(opts.ServiceName)
	if serviceName == "" {
		serviceName = DefaultServiceName
	}

	cfg := &Config{
		Observability: observability.Config{
			// slog logging to stdout as structured JSON.
			Logging: loggingcfg.Config{
				Provider:    loggingcfg.ProviderSlog,
				ServiceName: serviceName,
				Level:       levelFromString(opts.LogLevel),
			},
			// Tracing, Metrics, and Profiling are left at their zero values,
			// which the platform resolves to noop providers. Enable them by
			// populating the corresponding sub-config.
		},
	}

	return cfg
}

// envVarOptions returns the platform config options shared by every loader in
// this package: the application's env var prefix and a debug hook that logs each
// value the parser applies. Because the hook uses the standard-library slog
// default logger, these lines appear only when that logger is at debug level.
func envVarOptions() []platformconfig.Option {
	return []platformconfig.Option{
		platformconfig.WithPrefix(EnvVarPrefix),
		platformconfig.WithOnSet(func(tag string, value any, isDefault bool) {
			slog.Debug("config value set from environment",
				slog.String("tag", tag),
				slog.Any("value", value),
				slog.Bool("isDefault", isDefault),
			)
		}),
	}
}

// Load builds a Config from the given options and then overlays environment
// variables on top of it. The options (typically the CLI's flags) seed the
// defaults; any TEMPLATE_GO_-prefixed environment variable that is set wins over
// them. Fields left unset in the environment keep their default value, so the
// binary still boots with structured slog logging and noop telemetry out of the
// box. The result is validated before it is returned.
func Load(ctx context.Context, opts Options) (*Config, error) {
	cfg := New(opts)

	if err := platformconfig.ApplyEnvironmentVariables(cfg, envVarOptions()...); err != nil {
		return nil, fmt.Errorf("applying environment variables: %w", err)
	}

	if err := cfg.Validate(ctx); err != nil {
		return nil, fmt.Errorf("validating configuration: %w", err)
	}

	return cfg, nil
}

// LoadFromFile decodes a complete JSON configuration file and then overlays
// environment variables (a set TEMPLATE_GO_ variable wins over the file value).
// Unlike Load, it does not start from the built-in defaults: the file is
// expected to fully specify the config — at minimum a logging service name — so
// use it once a deployment has a real config file to mount. The result is
// validated before it is returned.
func LoadFromFile(ctx context.Context, path string) (*Config, error) {
	cfg, err := platformconfig.LoadFromJSONFile[Config](path, envVarOptions()...)
	if err != nil {
		return nil, fmt.Errorf("loading configuration file: %w", err)
	}

	if err = cfg.Validate(ctx); err != nil {
		return nil, fmt.Errorf("validating configuration: %w", err)
	}

	return cfg, nil
}

// Validate confirms the assembled configuration is internally consistent.
func (c *Config) Validate(ctx context.Context) error {
	return c.Observability.ValidateWithContext(ctx)
}

// NewPillars builds the observability pillars for the application.
//
// Logging is configured from Config (structured slog by default), while
// tracing, metrics, and profiling default to noop providers so the binary stays
// quiet and dependency-free out of the box. To enable real telemetry, populate
// the Tracing/Metrics/Profiling sub-configs of c.Observability and call
// c.Observability.NewPillars(ctx) instead — it wires OTel/Cloud providers from
// the same config — or replace the noop constructors below with your own.
func (c *Config) NewPillars(ctx context.Context) (*observability.Pillars, error) {
	logger, err := c.Observability.Logging.NewLogger(ctx)
	if err != nil {
		return nil, err
	}

	return &observability.Pillars{
		Logger:          logger,
		TracerProvider:  tracingnoop.NewTracerProvider(),
		MetricsProvider: metricsnoop.NewMetricsProvider(),
		Profiler:        profilingnoop.NewProvider(),
	}, nil
}

// levelFromString maps a human-friendly level name onto a platform log level,
// defaulting to info for empty or unrecognized input.
func levelFromString(s string) logging.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case LevelDebug:
		return logging.DebugLevel
	case LevelWarn, "warning":
		return logging.WarnLevel
	case LevelError:
		return logging.ErrorLevel
	default:
		return logging.InfoLevel
	}
}
