#!/usr/bin/env bash
# Runs one lwwbench workload against redis-server with tairhash loaded and records a CPU
# profile of the server (perf on Linux, sample on macOS) plus throughput, server CPU and
# memory. See bench/README.md.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

SERVER_SRC="$ROOT/.deps/redis-7.0"
MODULE="$ROOT/build/scan/lib/tairhash_module.so"
WORKLOAD="lww"
PRELOAD="load"
KEYS=100000
FIELDS=30
OPS=4000000
PIPELINE=64
STALE=10
SECONDS_TO_PROFILE=5
PORT=27101
OUT=""

usage() {
    cat <<'EOF'
Usage: bench/profile.sh [options]

  --server-src DIR   Built server source tree (default: .deps/redis-7.0, see tests/run.sh)
  --module PATH      Module to load (default: build/scan/lib/tairhash_module.so)
  --workload W       lwwbench workload (default: lww)
  --preload W        Population workload run first: load, loadset, loadhset or none (default: load)
  --keys N           Keys (default: 100000)
  --fields N         Fields per key (default: 30)
  --ops N            Operations (default: 4000000); size it to run longer than the profile
  --pipeline N       Pipeline depth (default: 64)
  --stale PCT        Share of lww/gt writes carrying an old version (default: 10)
  --seconds N        Profile length (default: 5)
  --port N           Server port (default: 27101)
  --out DIR          Output directory (default: build/profile/<workload>)
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --server-src) SERVER_SRC="$2"; shift 2 ;;
        --module) MODULE="$2"; shift 2 ;;
        --workload) WORKLOAD="$2"; shift 2 ;;
        --preload) PRELOAD="$2"; shift 2 ;;
        --keys) KEYS="$2"; shift 2 ;;
        --fields) FIELDS="$2"; shift 2 ;;
        --ops) OPS="$2"; shift 2 ;;
        --pipeline) PIPELINE="$2"; shift 2 ;;
        --stale) STALE="$2"; shift 2 ;;
        --seconds) SECONDS_TO_PROFILE="$2"; shift 2 ;;
        --port) PORT="$2"; shift 2 ;;
        --out) OUT="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done

OUT="${OUT:-$ROOT/build/profile/$WORKLOAD}"
SERVER="$SERVER_SRC/src/redis-server"
CLI="$SERVER_SRC/src/redis-cli"
BENCH="$ROOT/build/lwwbench"
HIREDIS="$SERVER_SRC/deps/hiredis"
[[ -x "$SERVER" ]] || { echo "no server at $SERVER; build one with tests/run.sh" >&2; exit 1; }

if [[ ! -x "$BENCH" || "$ROOT/bench/lwwbench.c" -nt "$BENCH" ]]; then
    mkdir -p "$(dirname "$BENCH")"
    "${CC:-cc}" -O2 -Wall -Wextra -I"$HIREDIS" "$ROOT/bench/lwwbench.c" "$HIREDIS/libhiredis.a" -o "$BENCH"
fi

rm -rf "$OUT" && mkdir -p "$OUT"
server_args=(--port "$PORT" --save '' --appendonly no --daemonize yes --logfile "$OUT/server.log" --dir "$OUT" --pidfile "$OUT/server.pid")
[[ "$MODULE" != none ]] && server_args+=(--loadmodule "$MODULE")
"$SERVER" "${server_args[@]}"
trap '"$CLI" -p "$PORT" shutdown nosave >/dev/null 2>&1 || true' EXIT
for _ in $(seq 50); do
    [[ -s "$OUT/server.pid" ]] && "$CLI" -p "$PORT" ping >/dev/null 2>&1 && break
    sleep 0.1
done
PID="$(cat "$OUT/server.pid")"

mem() { "$CLI" -p "$PORT" info memory | awk -F: -v k="$1" '$1 == k { gsub("\r", ""); print $2 }'; }
empty_mem="$(mem used_memory)"
if [[ "$PRELOAD" != none ]]; then
    "$BENCH" -p "$PORT" -w "$PRELOAD" -k "$KEYS" -f "$FIELDS" > "$OUT/preload.txt"
fi
loaded_mem="$(mem used_memory)"

"$BENCH" -p "$PORT" -w "$WORKLOAD" -k "$KEYS" -f "$FIELDS" -n "$OPS" -P "$PIPELINE" -s "$STALE" > "$OUT/bench.txt" 2> "$OUT/bench.err" &
bench_pid=$!
sleep 1
cpu_start="$(ps -o %cpu= -p "$PID" | tr -d ' ')"
if [[ "$(uname -s)" == "Darwin" ]]; then
    sample "$PID" "$SECONDS_TO_PROFILE" 1 -file "$OUT/sample.txt" > /dev/null 2>&1
    profile="$OUT/sample.txt"
else
    perf record -F 999 -g -p "$PID" -o "$OUT/perf.data" -- sleep "$SECONDS_TO_PROFILE" > /dev/null 2>&1
    perf report -i "$OUT/perf.data" --no-children --stdio > "$OUT/perf-flat.txt" 2> /dev/null
    perf report -i "$OUT/perf.data" --children --stdio > "$OUT/perf-children.txt" 2> /dev/null
    profile="$OUT/perf-flat.txt (and perf-children.txt)"
fi
cpu_end="$(ps -o %cpu= -p "$PID" | tr -d ' ')"
wait "$bench_pid"

keys_loaded="$("$CLI" -p "$PORT" dbsize)"
echo "$(cat "$OUT/bench.txt") server_cpu_pct=$cpu_start,$cpu_end"
if [[ "$keys_loaded" -gt 0 ]]; then
    echo "memory: used_memory_per_key=$(( (loaded_mem - empty_mem) / keys_loaded )) after preload of $keys_loaded keys"
fi
echo "profile: $profile"
