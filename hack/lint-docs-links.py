#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

"""Lint Markdown docs for incorrect ovn-kubernetes.io link conventions.

Exits 0 when every link follows the conventions, 1 otherwise.

Conventions (documented in docs/developer-guide/documentation.md,
"Linking between docs pages"):

  docs/ files (except governance symlinks to the repo root)
      Cross-reference other docs pages with relative Markdown paths.
      Do NOT use https://ovn-kubernetes.io/… URLs for in-docs links.

  Governance symlinks (repo-root files served via docs/governance/)
  and other repo-root Markdown files
      Deep-links into the published site MUST include a version prefix
      (master/, latest/, or a release number like 1.3/).

  A bare homepage link — https://ovn-kubernetes.io or
  https://ovn-kubernetes.io/ with nothing after the slash — is always
  fine everywhere.

Links inside fenced code blocks are ignored (they are documentation
examples, not live references).
"""

import os
import re
import sys
from pathlib import Path

# Inline Markdown link: [text](https://ovn-kubernetes.io/…)
# Also matches angle-bracket form: [text](<https://ovn-kubernetes.io/…>)
_INLINE_LINK_RE = re.compile(
    r"\]\(\s*<?(https?://(?:www\.)?ovn-kubernetes\.io(?:/[^)\s>]*)?)>?"
)

# Reference-style link definition: [label]: https://ovn-kubernetes.io/…
# Also matches angle-bracket form: [label]: <https://ovn-kubernetes.io/…>
_REF_DEF_RE = re.compile(
    r"^\s{0,3}\[.*\]:\s+<?(https?://(?:www\.)?ovn-kubernetes\.io(?:/[^\s>]*)?)>?",
    re.MULTILINE,
)

# Versioned path prefix: /master/…, /latest/…, /1.3/… etc
_VERSION_PREFIX_RE = re.compile(r"^/(master|latest|\d+\.\d+)(/|$)")

# Fenced code block delimiter
_FENCE_OPEN_RE = re.compile(r"^(\s*(`{3,}|~{3,}))")

def _fenced_line_set(lines):
    """Return the set of 0-based line indices that are inside fenced code blocks."""
    inside = set()
    in_fence = False
    fence_char = None
    fence_indent = 0
    fence_len = 0
    for idx, line in enumerate(lines):
        stripped = line.lstrip()
        m = _FENCE_OPEN_RE.match(line)
        if m:
            char = m.group(2)[0]
            length = len(m.group(2))
            indent = len(line) - len(stripped)
            if not in_fence:
                in_fence = True
                fence_char = char
                fence_indent = indent
                fence_len = length
                continue
            if char == fence_char and length >= fence_len and indent <= fence_indent:
                in_fence = False
                continue
        if in_fence:
            inside.add(idx)
    return inside


def _extract_site_path(url):
    """Return the path portion after the hostname, or None for a bare homepage"""
    remainder = re.sub(r"^https?://(?:www\.)?ovn-kubernetes\.io", "", url)
    if not remainder or remainder == "/":
        return None
    return remainder


def _is_governance_symlink(filepath, docs_dir):
    """True when filepath is a symlink under docs/governance/ pointing outside docs/."""
    governance_dir = docs_dir / "governance"
    try:
        filepath.relative_to(governance_dir)
    except ValueError:
        return False
    if not filepath.is_symlink():
        return False
    resolved = filepath.resolve()
    try:
        resolved.relative_to(docs_dir.resolve())
        return False
    except ValueError:
        return True

def lint_file(filepath, repo_root):
    """Return a list of ``(line_number, message)`` tuples for violations."""
    docs_dir = repo_root / "docs"
    errors = []

    try:
        text = filepath.read_text(encoding="utf-8", errors="replace")
    except OSError as exc:
        return [(0, f"cannot read file: {exc}")]

    lines = text.splitlines()
    fenced = _fenced_line_set(lines)

    # Determine which rule set applies.
    resolved = filepath.resolve()
    try:
        resolved.relative_to(docs_dir.resolve())
        is_docs_file = True
    except ValueError:
        is_docs_file = False

    is_gov_symlink = _is_governance_symlink(filepath, docs_dir)

    require_relative = is_docs_file and not is_gov_symlink

    for idx, line in enumerate(lines):
        if idx in fenced:
            continue
        for m in list(_INLINE_LINK_RE.finditer(line)) + list(
            _REF_DEF_RE.finditer(line)
        ):
            url = m.group(1)
            site_path = _extract_site_path(url)
            if site_path is None:
                continue

            if require_relative:
                errors.append(
                    (idx + 1, f"use a relative Markdown path instead of {url}")
                )
            else:
                if not _VERSION_PREFIX_RE.match(site_path):
                    errors.append(
                        (
                            idx + 1,
                            f"use a versioned URL (e.g. /master/…) instead of {url}",
                        )
                    )

    return errors

def main(argv=None):
    repo_root = Path(__file__).resolve().parent.parent

    docs_dir = repo_root / "docs"
    targets = []

    for md in sorted(docs_dir.rglob("*.md")):
        targets.append(md)

    for md in sorted(repo_root.glob("*.md")):
        targets.append(md)

    total_errors = 0
    for filepath in targets:
        errors = lint_file(filepath, repo_root)
        for lineno, msg in errors:
            relpath = filepath.relative_to(repo_root)
            print(f"{relpath}:{lineno}: {msg}")
            total_errors += 1

    if total_errors:
        print(f"\n{total_errors} link convention violation(s) found.")
        print(
            'See docs/developer-guide/documentation.md, "Linking between docs pages".'
        )
        return 1

    print("All docs links follow conventions. ✓")
    return 0


if __name__ == "__main__":
    sys.exit(main())
