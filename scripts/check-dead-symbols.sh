#!/usr/bin/env bash
# check-dead-symbols.sh — guard against reintroduction of removed symbols.
#
# Reads scripts/dead-symbols.json and scans the repo with PCRE grep.
# Each catalog entry has:
#   - symbol:    short human label
#   - patterns:  array of PCRE regexes joined with `|`
#   - allow_paths: array of path-regexes where the symbol is allowed to remain
#   - desc:      reason for removal (printed when a hit surfaces)
#
# Exits 0 if all matches (if any) fall within allow_paths.
# Exits 1 if any non-allow-listed reference is found.
# Exits 2 if grep, jq, or the catalog are missing.
#
# Usage: ./scripts/check-dead-symbols.sh [ROOT]
#   Default ROOT: the project root (parent of this script's directory).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${1:-$(cd "$SCRIPT_DIR/.." && pwd)}"
CATALOG="$ROOT/scripts/dead-symbols.json"

# --- Tool availability ---
if ! command -v grep >/dev/null 2>&1; then
    echo "[ERROR] GNU grep is required but not in PATH" >&2
    exit 2
fi
if ! command -v jq >/dev/null 2>&1; then
    echo "[ERROR] jq is required but not in PATH" >&2
    exit 2
fi
if [[ ! -f "$CATALOG" ]]; then
    echo "[ERROR] catalog not found at $CATALOG" >&2
    exit 2
fi

cd "$ROOT"

# Top-level entries excluded from scanning.
#   .git/  — VC internals, incl. binary pack files; no useful surface.
#   target/ — Rust build artifacts; ditto.
# New entries here MUST be regex-safe names (no unescaped metacharacters).
TOP_LEVEL_EXCLUDES=(".git" "target")

TOTAL=$(jq 'length' "$CATALOG")
FAILED=0

for ((i = 0; i < TOTAL; i++)); do
    DESC=$(jq -r ".[$i].desc" "$CATALOG")
    PATTERNS=$(jq -r ".[$i].patterns | join(\"|\")" "$CATALOG")
    ALLOW_PATHS=$(jq -r ".[$i].allow_paths | join(\"|\")" "$CATALOG")

    # Build the search-root argument list. `ls -A` gives every top-level entry
    # including hidden ones; we then strip the entries in TOP_LEVEL_EXCLUDES.
    # This is portable and avoids grep traversing binary pack files in .git/.
    set +e
    EXCLUDE_RE="$(IFS='|'; printf '%s' "${TOP_LEVEL_EXCLUDES[*]}")"
    SCAN_TARGETS=$(ls -A "$ROOT" 2>/dev/null | grep -vE "^(${EXCLUDE_RE})\$" | tr '\n' ' ')
    [[ -z "$SCAN_TARGETS" ]] && SCAN_TARGETS="."
    HITS=$(grep -RPn --exclude=dead-symbols.json -e "$PATTERNS" $SCAN_TARGETS 2>/dev/null)
    GR_EXIT=$?
    set -e
    # grep exit 0 = matches, 1 = no matches, 2+ = real error.
    if [[ $GR_EXIT -gt 1 ]]; then
        echo "[ERROR] grep failed (exit=$GR_EXIT) on pattern '$PATTERNS'" >&2
        exit 2
    fi
    if [[ -z "$HITS" ]]; then
        continue
    fi

    # Filter out allow-listed paths. grep without an explicit `./` search
    # root does NOT prefix output paths — but we still tolerate the `./`
    # prefix in the regex so the filter is correct across tool conventions.
    if [[ -n "$ALLOW_PATHS" ]]; then
        FILTERED=$(echo "$HITS" | grep -vE "^(\./)?(${ALLOW_PATHS}):" || true)
    else
        FILTERED="$HITS"
    fi
    if [[ -z "$FILTERED" ]]; then
        continue
    fi

    echo "[VIOLATION] removed symbol referenced outside allow-listed paths"
    echo "  Reason: $DESC"
    echo "  Hits:"
    echo "$FILTERED" | sed 's/^/    /'
    echo ""
    FAILED=1
done

if [[ "$FAILED" -eq 1 ]]; then
    echo "[FAIL] removed symbol(s) still referenced. Update the catalog or the code."
    exit 1
fi

echo "[OK] no removed symbols referenced (catalog: scripts/dead-symbols.json, entries: $TOTAL)"
exit 0
