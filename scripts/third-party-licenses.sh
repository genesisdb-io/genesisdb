#!/usr/bin/env sh
# Writes THIRD_PARTY_LICENSES: every module compiled into the genesisdb
# binary with its license, followed by the full license texts. Every build
# runs it (make build, make release-binaries, the Dockerfile), the file is
# not checked in. CI runs it to reject licenses outside the allowed set.
set -eu
cd "$(dirname "$0")/.."
out="${1:-THIRD_PARTY_LICENSES}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# go-licenses warns about assembly files it cannot inspect, that noise goes
# to a log file and is shown only on failure.
run() { if ! "$@" 2>"$tmp/log"; then cat "$tmp/log" >&2; exit 1; fi; }
run go-licenses check ./cmd/genesisdb --allowed_licenses=MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC
run go-licenses report ./cmd/genesisdb > "$tmp/report.csv"
run go-licenses save ./cmd/genesisdb --save_path="$tmp/texts" --force

{
  echo "Third-party licenses"
  echo "===================="
  echo
  echo "GenesisDB is released under the MIT License (see LICENSE). The genesisdb"
  echo "binary links the following Go modules. Each is listed with its license,"
  echo "the full license texts follow."
  echo
  grep -v '^github.com/genesisdb-io/genesisdb' "$tmp/report.csv" | sort -u | while IFS=, read -r module url license; do
    printf '%-72s %s\n' "$module" "$license"
  done
  echo
  find "$tmp/texts" -type f | sort | while read -r file; do
    module="${file#"$tmp/texts/"}"
    module="$(dirname "$module")"
    echo
    echo "--------------------------------------------------------------------------------"
    echo "$module"
    echo "--------------------------------------------------------------------------------"
    echo
    cat "$file"
    echo
  done
} > "$out"
echo "wrote $out ($(grep -c . "$out") lines)"
