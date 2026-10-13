#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
#
# Verify that docs/, mkdocs.yml nav:, and section index.md card grids stay
# in sync. Intended to run in CI (docs.yml) so contributors get fast feedback
# when a new page is added without updating all three.
#
# Checks performed:
#   1. Every .md path in mkdocs.yml nav: must exist on disk.
#   2. Every nav entry whose parent directory contains a card-grid landing
#      page (index.md with a landing-grid div) must be referenced inside
#      that index.md's card grid .
#
# Sub-section pages (e.g. observability/metrics/ovn.md) are reachable
# through a parent card linking to the sub-section overview, so only
# direct children of a card-grid directory are checked.
#
# Exits 0 when everything is in sync, 1 on mismatches.
#
# Usage (from repo root):
#   ./hack/verify-docs-nav-sync.sh

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MKDOCS_YML="${ROOT_DIR}/mkdocs.yml"
DOCS_DIR="${ROOT_DIR}/docs"

errors=0

# ── 1. Extract every .md path from non-commented nav: lines ─────────────────
# The range /^nav:/,/^[a-zA-Z]/ captures from nav: up to (and including) the
# next top-level YAML key, which contains no .md paths so the result is safe.
# This avoids breaking when nav items start at column zero (e.g. "- Home:").
mapfile -t nav_files < <(
  sed -n '/^nav:/,/^[a-zA-Z]/p' "${MKDOCS_YML}" \
    | grep -v '^\s*#' \
    | grep -oE '[A-Za-z0-9_./-]+\.md' \
    | sort -u
)

# ── 2. Every nav .md must exist on disk ──────────────────────────────────────
for nav_entry in "${nav_files[@]}"; do
  if [[ ! -f "${DOCS_DIR}/${nav_entry}" ]]; then
    echo "ERROR: mkdocs.yml nav: references '${nav_entry}' but docs/${nav_entry} does not exist" >&2
    errors=$((errors + 1))
  fi
done

# ── 3. Every direct-child nav entry must appear in its section index.md ──────
#
# A "direct child" means the file sits in the same directory as the
# card-grid index.md. Files in sub-directories (e.g.
# observability/metrics/ovn.md) are reachable via a card that links to
# their sub-section overview and are NOT required to have individual cards.
for nav_entry in "${nav_files[@]}"; do
  # Skip the root index.
  [[ "${nav_entry}" == "index.md" ]] && continue

  # Skip blog posts — managed by the Material blog plugin.
  [[ "${nav_entry}" == blog/* ]] && continue

  file_dir="$(dirname "${nav_entry}")"

  # For subsection index.md files (e.g. features/bgp-integration/index.md),
  # check against the parent directory's card grid (features/index.md).
  # A section's own index.md is skipped only when its directory has no
  # parent card grid — that page IS a top-level landing page.
  if [[ "$(basename "${nav_entry}")" == "index.md" ]]; then
    parent_dir="$(dirname "${file_dir}")"
    index_candidate="${DOCS_DIR}/${parent_dir}/index.md"
    [[ -f "${index_candidate}" ]] || continue
    grep -q 'landing-grid' "${index_candidate}" || continue
    # Cards link as "subdir/index.md", e.g. "bgp-integration/index.md"
    basename="$(basename "${file_dir}")/index.md"
    check_dir="${parent_dir}"
  else
    index_candidate="${DOCS_DIR}/${file_dir}/index.md"
    # Only check when the file's own directory has a card-grid index.md.
    [[ -f "${index_candidate}" ]] || continue
    grep -q 'landing-grid' "${index_candidate}" || continue
    basename="$(basename "${nav_entry}")"
    check_dir="${file_dir}"
  fi

  # Extract only the landing-grid section so links in intro text don't
  # cause false passes. The grid is always the last block in the file.
  grid_section="$(sed -n '/landing-grid/,$p' "${index_candidate}")"

  if ! echo "${grid_section}" | grep -qF "](${basename})" \
     && ! echo "${grid_section}" | grep -qF "](${basename}#"; then
    echo "ERROR: '${nav_entry}' is in mkdocs.yml nav: but not referenced in docs/${check_dir}/index.md" >&2
    errors=$((errors + 1))
  fi
done

if [[ "${errors}" -gt 0 ]]; then
  echo >&2
  echo "Found ${errors} docs navigation sync error(s)." >&2
  echo "When adding a new page, update both mkdocs.yml nav: AND the section index.md card grid." >&2
  echo "See docs/developer-guide/documentation.md for details." >&2
  exit 1
fi

echo "docs nav sync check passed."
