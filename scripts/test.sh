#!/usr/bin/env bash
set -euo pipefail

# Run tests
# Usage: test.sh

# shellcheck disable=SC2086,SC2046
CGO_ENABLED=1 go test -shuffle=on -race -vet=all -failfast $(go list github.com/primandproper/template-go/... | grep -Ev '(cmd)')
