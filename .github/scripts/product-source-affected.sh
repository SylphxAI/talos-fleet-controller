#!/usr/bin/env bash
set -euo pipefail

output_file="${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

ref_name="${GITHUB_REF_NAME:-${GITHUB_REF#refs/heads/}}"

if [ "${GITHUB_EVENT_NAME:-}" = "push" ] && { [ "$ref_name" = "main" ] || [ "$ref_name" = "dev" ]; }; then
  echo "product_changed=true" >> "$output_file"
  echo "Protected branch push; running product checks."
  exit 0
fi

base_ref="${GITHUB_BASE_REF:-main}"
git fetch --no-tags --depth=1 origin "$base_ref"

changed_files="$(git diff --name-only "origin/${base_ref}" HEAD)"

if [ -z "$changed_files" ]; then
  echo "product_changed=true" >> "$output_file"
  echo "No changed files detected; running product checks."
  exit 0
fi

product_files="$(printf '%s\n' "$changed_files" | grep -Ev '^(PROJECT\.md|AGENTS\.md|CLAUDE\.md|\.github/workflows/(build|lint|test|test-e2e)\.yml|\.github/scripts/product-source-affected\.sh)$' || true)"

if [ -z "$product_files" ]; then
  echo "product_changed=false" >> "$output_file"
  echo "No product source changes detected:"
  printf '%s\n' "$changed_files"
else
  echo "product_changed=true" >> "$output_file"
  echo "Product source changes detected:"
  printf '%s\n' "$product_files"
fi
