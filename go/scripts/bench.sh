#!/usr/bin/env bash
# Benchmarks the fieldhash backends end to end: starts a plain Valkey for the lua,
# native and plain-hash backends and one otherwise identical Valkey per TairHash
# module build, runs the load sweep (measured repeats, then one profiled pass),
# the memory scenario and the reclaim scenario, then renders the report. Servers
# are stopped on exit.
#
#   bench.sh OUT_DIR [extra thbench run flags...]
#
# Environment:
#   MODULE_SO        module to load when MODULES is unset
#                    (default .cache/modules/local-scan/tairhash_module.so)
#   MODULES          space-separated name=path.so module builds to compare, each on
#                    its own server and benchmarked as backend <name> (default:
#                    module=$MODULE_SO). Names match [a-z0-9][a-z0-9._+-]*.
#   MODULE_ARGS      module load arguments for every build, e.g. "active_expire_period 100"
#   PLAIN_BACKENDS   backends run on the plain server (default "lua native hash";
#                    empty runs no plain server)
#   LABEL            label recorded with every result (default with one module:
#                    tairhash@<revision from source-revision next to its .so>,
#                    with several: "modules <names>"; MODULE_ARGS appended)
#   REFERENCE        backend the report compares the others against (default:
#                    module when present, else the first backend)
#   PLAIN_PORT       plain server port (default 26379)
#   MODULE_PORT      first module server port; build i listens on MODULE_PORT+i (default 26380)
#   RECLAIM_ARGS     extra thbench reclaim flags
#   SKIP_LOAD=1      skip the load sweep (for example a reclaim-only pass with other MODULE_ARGS)
#   SKIP_MEMORY=1    skip the memory scenario (memory per field with and without TTLs)
#   MEMORY_ARGS      extra thbench memory flags
#   SKIP_RECLAIM=1   skip the reclaim scenario
#   SKIP_PROFILE=1   skip the profiled pass (client pprof + server sample/perf)
#   CLIENT_QOS       macOS QoS clamp for the client (default utility; empty disables)
#   CLIENT_CPUS      Linux: taskset CPU list for the client (default unpinned)
#   SERVER_CPUS      Linux: taskset CPU list for both servers (default unpinned)
#
# Output: results.jsonl, report.md, runs/<label>/<run>/ (INFO before/after,
# CONFIG, MEMORY STATS, slowlog, latency events, timeline, profiles), logs, host
# load. Results append, so several invocations into one OUT_DIR (one workload
# each, say) share a report. The report is rendered from whatever results exist
# even when a phase fails; the script still exits with that phase's status.
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

plain_port="${PLAIN_PORT:-26379}"
module_port="${MODULE_PORT:-26380}"
read -r -a plain_backends <<< "${PLAIN_BACKENDS-lua native hash}"
read -r -a modules <<< "${MODULES:-module=${MODULE_SO:-$here/.cache/modules/local-scan/tairhash_module.so}}"
cli="$here/.cache/valkey-${VALKEY_VERSION:-9.1.3}/src/valkey-cli"
thbench="$here/.cache/thbench"

module_names=()
module_sos=()
for m in "${modules[@]}"; do
  name="${m%%=*}" so="${m#*=}"
  [[ "$m" == *=* && -n "$name" && -n "$so" ]] || { echo "MODULES entry $m is not name=path.so" >&2; exit 1; }
  [[ -f "$so" ]] || { echo "module $name not built: $so (run make module)" >&2; exit 1; }
  module_names+=("$name")
  module_sos+=("$so")
done

# revision prints the commit (7 characters, +dirty for uncommitted changes) a
# module was built from, from the source-revision file make module writes.
revision() {
  local rev_file
  rev_file="$(dirname "$1")/source-revision"
  if [[ ! -f "$rev_file" ]]; then echo unknown; return; fi
  local rev
  rev="$(head -n1 "$rev_file" | cut -c1-7)"
  if [[ $(wc -l < "$rev_file") -gt 1 ]]; then rev+="+dirty"; fi
  echo "$rev"
}

label="${LABEL:-}"
if [[ -z "$label" ]]; then
  if [[ ${#modules[@]} -eq 1 ]]; then
    label="tairhash@$(revision "${module_sos[0]}")"
  else
    label="modules ${module_names[*]}"
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

backends=()
ports=()
for i in "${!module_names[@]}"; do
  port=$((module_port + i))
  # shellcheck disable=SC2086 # MODULE_ARGS is a word list by design
  "${server_cmd[@]+"${server_cmd[@]}"}" "$here/scripts/valkey-server.sh" "$port" "$out/server-${module_names[$i]}" \
    --loadmodule "${module_sos[$i]}" ${MODULE_ARGS:-} &
  pids+=($!)
  ports+=("$port")
  backends+=("${module_names[$i]}=module@127.0.0.1:$port")
done
if [[ ${#plain_backends[@]} -gt 0 ]]; then
  "${server_cmd[@]+"${server_cmd[@]}"}" "$here/scripts/valkey-server.sh" "$plain_port" "$out/server-plain" &
  pids+=($!)
  ports+=("$plain_port")
  for b in "${plain_backends[@]}"; do backends+=("$b@127.0.0.1:$plain_port"); done
fi

for port in "${ports[@]}"; do
  for _ in $(seq 100); do
    "$cli" -p "$port" PING >/dev/null 2>&1 && break
    sleep 0.1
  done
  "$cli" -p "$port" PING >/dev/null || { echo "server on $port did not start" >&2; exit 1; }
done

backend_list="$(IFS=,; echo "${backends[*]}")"
{
  uptime
  echo "label: $label"
  echo "placement: $placement"
  for i in "${!module_names[@]}"; do
    echo "module ${module_names[$i]}: ${module_sos[$i]} ($(revision "${module_sos[$i]}")) on port ${ports[$i]}"
  done
  echo "backends: $backend_list"
} | tee -a "$out/host-load.txt"

profile_flag=(-profile)
[[ -n "${SKIP_PROFILE:-}" ]] && profile_flag=()

status=0
if [[ -z "${SKIP_LOAD:-}" ]]; then
  "${client_cmd[@]+"${client_cmd[@]}"}" "$thbench" run -flush -backends "$backend_list" -out "$out/results.jsonl" \
    -artifacts-dir "$out/runs" -label "$label" -placement "$placement" "${profile_flag[@]+"${profile_flag[@]}"}" "$@" \
    2> >(tee -a "$out/run.log" >&2) || status=$?
fi

if [[ $status -eq 0 && -z "${SKIP_MEMORY:-}" ]]; then
  # shellcheck disable=SC2086 # MEMORY_ARGS is a word list by design
  "${client_cmd[@]+"${client_cmd[@]}"}" "$thbench" memory -flush -backends "$backend_list" -out "$out/results.jsonl" \
    -artifacts-dir "$out/runs" -label "$label" -placement "$placement" ${MEMORY_ARGS:-} \
    2> >(tee -a "$out/memory.log" >&2) || status=$?
fi

if [[ $status -eq 0 && -z "${SKIP_RECLAIM:-}" ]]; then
  # shellcheck disable=SC2086 # RECLAIM_ARGS is a word list by design
  "${client_cmd[@]+"${client_cmd[@]}"}" "$thbench" reclaim -flush -backends "$backend_list" -out "$out/results.jsonl" \
    -artifacts-dir "$out/runs" -label "$label" -placement "$placement" ${RECLAIM_ARGS:-} \
    2> >(tee -a "$out/reclaim.log" >&2) || status=$?
fi

uptime | tee -a "$out/host-load.txt"
if [[ -s "$out/results.jsonl" ]]; then
  "$thbench" report -reference "${REFERENCE:-}" "$out/results.jsonl" > "$out/report.md"
  echo "report: $out/report.md"
fi
exit "$status"
