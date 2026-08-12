#!/usr/bin/env bash
# Fetch the esctest2 VT conformance suite and run it against the engine's VT.
#
# esctest2 (github.com/ThomasDickey/esctest2) is GPL-2.0; the engine is MPL-2.0.
# Those cannot be COMBINED into one work in this direction: MPL-2.0 §3.3 lets MPL
# files enter a GPL work, not GPL code enter an MPL one. So esctest2 is NOT
# vendored: this script fetches it into a gitignored checkout and the suite runs
# as a subprocess over a PTY (never linked into the engine), which is mere
# aggregation. See internal/esctest/harness.go and .kiro steering testing.md.
#
# Usage:
#   bash scripts/esctest.sh                 # run the conformance gate
#   bash scripts/esctest.sh -v              # verbose (list every FAIL)
#   ESCTEST_INCLUDE='CUPTests' bash scripts/esctest.sh -v   # one test class
#   ESCTEST_OPTIONS='none' bash scripts/esctest.sh          # drop the strictness opts
#
# The gate runs at maximum strictness by default (--options xtermWinopsEnabled;
# see internal/esctest/harness_test.go). It passes when the failing set matches
# internal/esctest/known_failures.txt (the allowlist of intentional
# deviations). To reseed that allowlist after a deliberate change, run with
# ESCTEST_LOGCOPY set and regenerate from the FAIL lines.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
dest="${ESCTEST2_DIR:-${repo_root}/.esctest2}"
# Pinned for a reproducible gate; bump deliberately when adopting newer tests.
pin="664be3c"
url="https://github.com/ThomasDickey/esctest2.git"

if ! command -v python3 >/dev/null 2>&1; then
  echo "esctest: python3 is required to run the suite" >&2
  exit 1
fi

if [ ! -d "${dest}/.git" ]; then
  echo "esctest: cloning ${url} -> ${dest}"
  git clone "${url}" "${dest}"
fi
git -C "${dest}" fetch --quiet origin
git -C "${dest}" checkout --quiet "${pin}"

echo "esctest: running conformance gate against ${dest}"
cd "${repo_root}"
ESCTEST2_DIR="${dest}" go test ./internal/esctest/ -run Conformance -timeout 1200s "$@"
