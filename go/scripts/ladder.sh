#!/usr/bin/env bash
# Benchmarks several TairHash revisions side by side with the lua and native
# backends. Each revision is built from its own clean clone, then must pass the
# Go integration suites (driver, fieldhash conformance, presence) with its build
# loaded; the benchmark runs only if every revision passes. The load sweep runs
# as one bench.sh invocation per workload, each taking the host lock on its
# own so other jobs can run between chunks, then one reclaim invocation; all
# append to OUT_DIR/results.jsonl and share OUT_DIR/report.md.
#
#   ladder.sh OUT_DIR name=ref [name=ref ...]
#
# name becomes the backend name (it must match [a-z0-9][a-z0-9._+-]*); ref is
# any commit-ish of TAIRHASH_REPO.
#
# Environment:
#   TAIRHASH_REPO        repository the refs resolve in (default: the checkout
#                        this harness lives in)
#   WORKLOADS            workloads, one chunk each (default: every workload)
#   CONCURRENCY          -concurrency of every chunk (default 16,128)
#   PROFILE_CONCURRENCY  levels the profiled pass covers (default: the last of CONCURRENCY)
#   REPEAT               measured repeats per cell (default 3)
#   REFERENCE            backend the report compares against (default: the first name)
#   LABEL                label of every result (default "ladder", plus MODULE_ARGS)
#   CONFORMANCE_PORT     port of the throwaway conformance server (default 26990)
#   SKIP_CONFORMANCE=1   benchmark without the conformance gate
#   SKIP_LOAD=1, SKIP_RECLAIM=1, SKIP_PROFILE=1   skip those phases
#   RUN_ARGS             extra thbench run flags for every chunk, e.g. "-duration 5s"
#   RECLAIM_ARGS         reclaim flags (default "-profile")
#   bench.sh's environment (MODULE_ARGS, PLAIN_BACKENDS, CLIENT_QOS, ...) passes through.
#
# Output in OUT_DIR, besides bench.sh's: ladder.txt (each name's ref, commit,
# subject, -march flags and conformance verdict) and conformance-<name>.log.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:?usage: ladder.sh OUT_DIR name=ref [name=ref ...]}"
shift
[[ $# -gt 0 ]] || { echo "usage: ladder.sh OUT_DIR name=ref [name=ref ...]" >&2; exit 2; }
mkdir -p "$out"
out="$(cd "$out" && pwd)"
repo="${TAIRHASH_REPO:-$(cd "$here/.." && pwd)}"
concurrency="${CONCURRENCY:-16,128}"
profile_concurrency="${PROFILE_CONCURRENCY:-${concurrency##*,}}"
cli="$here/.cache/valkey-${VALKEY_VERSION:-9.1.3}/src/valkey-cli"

names=()
commits=()
for spec in "$@"; do
  name="${spec%%=*}" ref="${spec#*=}"
  if [[ "$spec" != *=* || ! "$name" =~ ^[a-z0-9][a-z0-9._+-]*$ || -z "$ref" ]]; then
    echo "$spec: want name=ref with a name matching [a-z0-9][a-z0-9._+-]*" >&2
    exit 2
  fi
  commit="$(git -C "$repo" rev-parse --verify --quiet "$ref^{commit}")" || { echo "$name: $ref is not a commit in $repo" >&2; exit 2; }
  names+=("$name")
  commits+=("$commit")
done

# Each revision builds in its own clone: the module's CMakeLists writes into the
# source tree, and the build records the clone's HEAD as its revision. --shared
# borrows the repository's objects instead of copying them.
make -C "$here" valkey >/dev/null
modules=()
for i in "${!names[@]}"; do
  name="${names[$i]}" commit="${commits[$i]}"
  src="$here/.cache/src/$name-${commit:0:12}"
  if [[ ! -d "$src" ]]; then
    git clone --quiet --shared --no-checkout "$repo" "$src"
    git -C "$src" checkout --quiet --detach "$commit"
  fi
  GATE_LOCK_WAIT=1 GATE_LOCK_CORES=4 gate-lock run "thbench-build-$name" -- \
    make -C "$here" module MODULE_SRC="$src" MODULE_NAME="$name" MODULE_JOBS=4 > "$out/build-$name.log" 2>&1 ||
    { echo "$name: build failed; see $out/build-$name.log" >&2; exit 1; }
  modules+=("$name=$here/.cache/modules/$name-scan/tairhash_module.so")
done

# conformance runs the integration suites against a throwaway server with one
# build loaded. The suites skip without a server, which `go test` still reports
# as ok, so any skip fails the gate.
conformance() {
  local name="$1" so="$2" port="${CONFORMANCE_PORT:-26990}" dir pid status=0
  dir="$(mktemp -d)"
  "$here/scripts/valkey-server.sh" "$port" "$dir" --loadmodule "$so" &
  pid=$!
  for _ in $(seq 100); do
    "$cli" -p "$port" PING >/dev/null 2>&1 && break
    sleep 0.1
  done
  (cd "$here" && VALKEY_ADDR="127.0.0.1:$port" GATE_LOCK_WAIT=1 gate-lock run-focused "thbench-conformance-$name" -- \
    go test -race -count=1 -v ./tairhash ./fieldhash/module ./fieldhash/lua ./fieldhash/native ./presence) \
    > "$out/conformance-$name.log" 2>&1 || status=$?
  if [[ $status -eq 0 ]] && grep -q -- '--- SKIP' "$out/conformance-$name.log"; then
    echo "$name: integration tests skipped; see $out/conformance-$name.log" >&2
    status=1
  fi
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  rm -rf "$dir"
  return "$status"
}

{
  echo "ladder $(date -u +%Y-%m-%dT%H:%M:%SZ) from $repo"
  echo "concurrency $concurrency, profiled at $profile_concurrency, repeat ${REPEAT:-3}, module args: ${MODULE_ARGS:-none}"
} | tee -a "$out/ladder.txt"
failed=()
for i in "${!names[@]}"; do
  name="${names[$i]}" commit="${commits[$i]}" so="${modules[$i]#*=}"
  verdict=skipped
  if [[ -z "${SKIP_CONFORMANCE:-}" ]]; then
    if conformance "$name" "$so"; then verdict=pass; else verdict=FAIL; failed+=("$name"); fi
  fi
  march="$( (grep -o -- '-march=[^ "]*' "$(dirname "$so")/compile_commands.json" || true) | sort -u | tr '\n' ' ')"
  printf '%s\t%s\t%s\tflags: %s\tconformance: %s\n' "$name" "${commit:0:12}" \
    "$(git -C "$repo" log -1 --format=%s "$commit")" "${march:-no -march}" "$verdict" | tee -a "$out/ladder.txt"
done
if [[ ${#failed[@]} -gt 0 ]]; then
  echo "conformance failed for: ${failed[*]}; see $out/conformance-<name>.log" >&2
  exit 1
fi

export MODULES="${modules[*]}"
export LABEL="${LABEL:-ladder${MODULE_ARGS:+ $MODULE_ARGS}}"
export REFERENCE="${REFERENCE:-${names[0]}}"
export GATE_LOCK_WAIT=1
read -r -a workloads <<< "$(echo "${WORKLOADS:-set-ttl get getall incr-ttl mixed presence cas}" | tr ',' ' ')"
if [[ -z "${SKIP_LOAD:-}" ]]; then
  for wl in "${workloads[@]}"; do
    echo "chunk $wl: $(date -u +%H:%M:%SZ)" | tee -a "$out/ladder.txt"
    # shellcheck disable=SC2086 # RUN_ARGS is a word list by design
    SKIP_RECLAIM=1 "$here/scripts/bench.sh" "$out" -workloads "$wl" -concurrency "$concurrency" \
      -repeat "${REPEAT:-3}" -profile-concurrency "$profile_concurrency" ${RUN_ARGS:-}
  done
fi
if [[ -z "${SKIP_RECLAIM:-}" ]]; then
  echo "chunk reclaim: $(date -u +%H:%M:%SZ)" | tee -a "$out/ladder.txt"
  SKIP_LOAD=1 RECLAIM_ARGS="${RECLAIM_ARGS--profile}" "$here/scripts/bench.sh" "$out"
fi
echo "done: $(date -u +%H:%M:%SZ)" | tee -a "$out/ladder.txt"
