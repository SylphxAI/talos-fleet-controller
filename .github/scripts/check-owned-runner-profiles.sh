#!/usr/bin/env bash
# Product CI accepts only explicit, company-owned runner profiles. Hosted,
# dynamic, generic self-hosted, legacy tuple, and invented selectors fail.
set -euo pipefail

workflow_dir="${1:-.github/workflows}"

if [[ ! -d "$workflow_dir" ]]; then
  printf 'owned runner selector guard: missing workflow directory: %s\n' "$workflow_dir" >&2
  exit 1
fi

selectors="$({
  find "$workflow_dir" -type f \( -name '*.yml' -o -name '*.yaml' \) -print0 |
    xargs -0 -r grep -HnE '^[[:space:]]*runs-on:[[:space:]]*' || true
} | sed -E 's/^[^:]+:[0-9]+:[[:space:]]*runs-on:[[:space:]]*//')"

if [[ -z "$selectors" ]]; then
  printf 'owned runner selector guard: no runs-on selectors found\n' >&2
  exit 1
fi

while IFS= read -r selector; do
  selector="$(printf '%s' "$selector" | sed -E 's/[[:space:]]+#.*$//; s/^[[:space:]]+//; s/[[:space:]]+$//')"
  case "$selector" in
    sylphx-linux-standard|sylphx-linux-large|sylphx-linux-xlarge|sylphx-linux-2xlarge)
      ;;
    *)
      printf 'owned runner selector guard: unsupported product selector: %s\n' "$selector" >&2
      exit 1
      ;;
  esac
done <<< "$selectors"

printf 'owned runner selector guard: all selectors are explicit Sylphx profiles\n'
