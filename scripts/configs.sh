#!/usr/bin/env bash
set -euo pipefail

# Render the per-environment config files from their real Go objects.
# Usage: configs.sh <module_path>
#   e.g. configs.sh github.com/primandproper/beeline

MODULE_PATH="${1:?missing module path}"

go run "${MODULE_PATH}/cmd/tools/codegen/configs"
