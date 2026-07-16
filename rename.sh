#!/usr/bin/env bash
set -euo pipefail

# rename.sh — retarget this template to your own module path, then delete itself.
#
# Usage: ./rename.sh <new-module-path>
#   e.g. ./rename.sh github.com/acme/coolapp
#
# It rewrites every reference to this template's module path
# (github.com/primandproper/template-go) and app identity (the bare "template-go"
# name and the TEMPLATE_GO_ env-var prefix) with values derived from your new
# module path, regenerates the checked-in config files and reformats the code,
# and finally removes this script — leaving no trace that the project started
# from a template.

OLD_MODULE="github.com/primandproper/template-go"
OLD_ORG="github.com/primandproper"
OLD_APP="template-go"
OLD_ENV_PREFIX="TEMPLATE_GO_"

NEW_MODULE="${1:-}"

if [[ -z "$NEW_MODULE" ]]; then
	echo "usage: ./rename.sh <new-module-path>   (e.g. github.com/acme/coolapp)" >&2
	exit 1
fi

if [[ "$NEW_MODULE" != */* ]]; then
	echo "error: <new-module-path> must be a full module path with at least one '/', e.g. github.com/acme/coolapp" >&2
	exit 1
fi

# Derive the org-level prefix (used for gci import grouping) and the short app name.
NEW_ORG="$(dirname "$NEW_MODULE")"
NEW_APP="$(basename "$NEW_MODULE")"
# Env-var prefix: uppercase the app name, map anything non-alphanumeric to '_'.
NEW_ENV_PREFIX="$(printf '%s' "$NEW_APP" | tr '[:lower:]' '[:upper:]' | tr -c '[:alnum:]' '_')_"

# Operate from the repository root (this script's directory) and never rewrite ourselves.
cd "$(dirname "$0")" || exit 1
SELF="$(basename "$0")"

echo "Retargeting template -> ${NEW_MODULE}"
echo "  module path : ${OLD_MODULE} -> ${NEW_MODULE}"
echo "  org prefix  : ${OLD_ORG} -> ${NEW_ORG}"
echo "  app name    : ${OLD_APP} -> ${NEW_APP}"
echo "  env prefix  : ${OLD_ENV_PREFIX} -> ${NEW_ENV_PREFIX}"

# Rewrite every text file that can carry a reference. find -print0 is portable
# (macOS/Linux), unlike grep -Z. Each perl substitution is ordered so the full
# module path is rewritten before the bare app name, and the org-prefix change is
# scoped to the gci config line — leaving platform-go's github.com/primandproper
# imports untouched. perl no-ops on files without a match.
count=0
while IFS= read -r -d '' file; do
	OM="$OLD_MODULE" NM="$NEW_MODULE" \
	OG="prefix(${OLD_ORG})" NG="prefix(${NEW_ORG})" \
	OE="$OLD_ENV_PREFIX" NE="$NEW_ENV_PREFIX" \
	OA="$OLD_APP" NA="$NEW_APP" \
		perl -i -pe '
			s/\Q$ENV{OM}\E/$ENV{NM}/g;
			s/\Q$ENV{OG}\E/$ENV{NG}/g;
			s/\Q$ENV{OE}\E/$ENV{NE}/g;
			s/\Q$ENV{OA}\E/$ENV{NA}/g;
		' "$file"
	count=$((count + 1))
done < <(find . -type f \
	\( -name '*.go' -o -name '*.mod' -o -name '*.md' -o -name '*.yml' -o -name '*.yaml' -o -name '*.sh' -o -name '*.json' -o -name 'Makefile' \) \
	-not -path './.git/*' -not -path './artifacts/*' -not -name "$SELF" -print0)

echo "Rewrote references across ${count} files."

# Tidy modules, regenerate configs, and reformat. The text rewrite above already
# leaves config/*.json correct; regenerating from the renamed Go source keeps
# them an authoritative projection when the toolchain is present. When the org
# prefix changes, platform-go moves import groups, so re-run the formatter too.
# All best-effort — the rename already stuck.
if command -v go >/dev/null 2>&1; then
	go mod tidy || echo "warning: 'go mod tidy' failed; run it manually" >&2
fi
if command -v make >/dev/null 2>&1; then
	make configs >/dev/null || echo "warning: 'make configs' failed; run it manually" >&2
	make format >/dev/null || echo "warning: 'make format' failed; run it manually" >&2
fi

echo "Done. Removing ${SELF}."
rm -f -- "$SELF"
