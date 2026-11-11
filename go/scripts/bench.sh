#!/usr/bin/env bash
# Benchmarks the three fieldhash backends end to end: starts a plain Valkey for the
# lua and native backends and a second, otherwise identical Valkey with the
# TairHash module, runs the load sweep (measured repeats, then one profiled pass)
# and the reclaim scenario, then renders the report. Servers are stopped on exit.
#
#   bench.sh OUT_DIR [extra thbench run flags...]
#
# Environment:
#   MODULE_SO        module to load (default .cache/module-scan/tairhash_module.so)
#   MODULE_ARGS      module load arguments, e.g. "active_expire_period 100"
#   LABEL            label recorded with every result (default: tairhash@<module
#                    revision from source-revision next to MODULE_SO> plus MODULE_ARGS)
#   PLAIN_PORT       plain server port (default 26379)
#   MODULE_PORT      module server port (default 26380)
#   RECLAIM_ARGS     extra thbench reclaim flags
#   SKIP_RECLAIM=1   skip the reclaim scenario
#   SKIP_PROFILE=1   skip the profiled pass (client pprof + server sample/perf)
#   CLIENT_QOS       macOS QoS clamp for the client (default utility; empty disables)
#   CLIENT_CPUS      Linux: taskset CPU list for the client (default unpinned)
#   SERVER_CPUS      Linux: taskset CPU list for both servers (default unpinned)
#
# Output: results.jsonl, report.md, runs/<label>/<run>/ (INFO before/after,
# CONFIG, MEMORY STATS, slowlog, latency events, timeline, profiles), logs, host
# load. The report is rendered from whatever results exist even when a phase
# fails; the script still exits with that phase's status.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:?usage: bench.sh OUT_DIR [thbench run flags...]}"
shift
mkdir -p "$out"
out="$(cd "$out" && pwd)"

# Benchmarks need the host to themselves; queue on the shared host lock when present.
if [[ -z "${BENCH_LOCKED:-}" ]] && command -v gate-lock >/dev/null 2>&1; then
  BENCH_LOCKED=1 exec gate-lock run-exclusive thbench -- "$0" "$out" "$@"
fi

module_so="${MODULE_SO:-$here/.cache/module-scan/tairhash_module.so}"
plain_port="${PLAIN_PORT:-26379}"
module_port="${MODULE_PORT:-26380}"
cli="$here/.cache/valkey-${VALKEY_VERSION:-9.1.3}/src/valkey-cli"
thbench="$here/.cache/thbench"

[[ -f "$module_so" ]] || { echo "module not built: $module_so (run make module)" >&2; exit 1; }

label="${LABEL:-}"
if [[ -z "$label" ]]; then
  rev_file="$(dirname "$module_so")/source-revision"
  label="tairhash@unknown"
  if [[ -f "$rev_file" ]]; then
    label="tairhash@$(head -n1 "$rev_file" | cut -c1-7)"
    if [[ $(wc -l < "$rev_file") -gt 1 ]]; then label+="+dirty"; fi
  fi
  if [[ -n "${MODULE_ARGS:-}" ]]; then label+=" ${MODULE_ARGS}"; fi
fi
(cd "$here" && go build -o "$thbench" ./cmd/thbench)

# The single-threaded server must not share cores with the load generator. On
# Apple silicon the scheduler cannot be pinned; a utility QoS clamp steers the
# client toward efficiency cores, leaving performance cores to the server, while
# still saturating it (a background clamp does not). On Linux, pin explicitly.
client_cmd=()
server_cmd=()
case "$(uname -s)" in
  Darwin)
    qos="${CLIENT_QOS-utility}"
    if [[ -n "$qos" ]]; then client_cmd=(taskpolicy -c "$qos"); fi
    ;;
  Linux)
    if [[ -n "${CLIENT_CPUS:-}" ]]; then client_cmd=(taskset -c "$CLIENT_CPUS"); fi
    if [[ -n "${SERVER_CPUS:-}" ]]; then server_cmd=(taskset -c "$SERVER_CPUS"); fi
    ;;
esac
placement="client: ${client_cmd[*]:-unbound}; servers: ${server_cmd[*]:-unbound}"

pids=()
# shellcheck disable=SC2329 # invoked by the trap below
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# shellcheck disable=SC2086 # MODULE_ARGS is a word list by design
"${server_cmd[@]+"${server_cmd[@]}"}" "$here/scripts/valkey-server.sh" "$module_port" "$out/server-module" --loadmodule "$module_so" ${MODULE_ARGS:-} &
pids+=($!)
"${server_cmd[@]+"${server_cmd[@]}"}" "$here/scripts/valkey-server.sh" "$plain_port" "$out/server-plain" &
pids+=($!)

for port in "$plain_port" "$module_port"; do
  for _ in $(seq 100); do
    "$cli" -p "$port" PING >/dev/null 2>&1 && break
    sleep 0.1
  done
  "$cli" -p "$port" PING >/dev/null || { echo "server on $port did not start" >&2; exit 1; }
done

backends="module@127.0.0.1:$module_port,lua@127.0.0.1:$plain_port,native@127.0.0.1:$plain_port"
uptime | tee "$out/host-load.txt"
echo "label: $label" | tee -a "$out/host-load.txt"
echo "placement: $placement" | tee -a "$out/host-load.txt"

profile_flag=(-profile)
[[ -n "${SKIP_PROFILE:-}" ]] && profile_flag=()

status=0
"${client_cmd[@]+"${client_cmd[@]}"}" "$thbench" run -flush -backends "$backends" -out "$out/results.jsonl" \
  -artifacts-dir "$out/runs" -label "$label" -placement "$placement" "${profile_flag[@]+"${profile_flag[@]}"}" "$@" \
  2> >(tee "$out/run.log" >&2) || status=$?

if [[ $status -eq 0 && -z "${SKIP_RECLAIM:-}" ]]; then
  # shellcheck disable=SC2086 # RECLAIM_ARGS is a word list by design
  "${client_cmd[@]+"${client_cmd[@]}"}" "$thbench" reclaim -flush -backends "$backends" -out "$out/results.jsonl" \
    -artifacts-dir "$out/runs" -label "$label" -placement "$placement" ${RECLAIM_ARGS:-} \
    2> >(tee "$out/reclaim.log" >&2) || status=$?
fi

uptime | tee -a "$out/host-load.txt"
if [[ -s "$out/results.jsonl" ]]; then
  "$thbench" report "$out/results.jsonl" > "$out/report.md"
  echo "report: $out/report.md"
fi
exit "$status"
