#!/usr/bin/env bash
#
# Regenerate the L1 CLI golden files from a reference C ostree binary.
#
# Usage:
#   OSTREE_BIN=/path/to/ostree go/hack/gen-cli-golden.sh
#
# The script always invokes the reference binary with argv[0] set to
# /usr/bin/ostree (via `exec -a`), so the goldens are independent of
# where the C binary actually lives.  The Go tests normalize argv[0] back
# to @PROG@ before diffing, so a Go binary invoked under any name can be
# compared against the same files.
#
# The case list lives in internal/cli/testdata/cases.tsv and is shared
# with the Go test, which never needs the C binary.

set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/.." && pwd)
testdata="$root/internal/cli/testdata"
cases="$testdata/cases.tsv"
ostree_bin=${OSTREE_BIN:-/usr/bin/ostree}

if [[ ! -x "$ostree_bin" ]]; then
  echo "error: reference ostree binary not executable: $ostree_bin" >&2
  exit 1
fi
if [[ ! -f "$cases" ]]; then
  echo "error: missing case list: $cases" >&2
  exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

export LC_ALL=C
# Force the default-repository lookup to fail deterministically.  Cases
# that care about the error message never print this path.
export OSTREE_REPO="$tmp/nonexistent-repo"
cd "$tmp"

# Provenance: record what the goldens were captured from.
( exec -a /usr/bin/ostree "$ostree_bin" --version ) >"$testdata/c-reference-version.txt"

count=0
while IFS=$'\t' read -r -a fields || [[ -n "${fields[0]:-}" ]]; do
  [[ ${#fields[@]} -eq 0 ]] && continue
  name=${fields[0]}
  [[ -z "$name" || "$name" == \#* ]] && continue
  args=("${fields[@]:1}")

  set +e
  ( exec -a /usr/bin/ostree "$ostree_bin" "${args[@]}" ) \
    >"$testdata/$name.out" 2>"$testdata/$name.err"
  status=$?
  set -e
  printf '%d\n' "$status" >"$testdata/$name.exit"
  count=$((count + 1))
done <"$cases"

echo "wrote $count golden cases to $testdata"
