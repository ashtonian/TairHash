#!/usr/bin/env bash
# Runs valkey-server in the foreground with the configuration every benchmark and
# test server shares, so the plain and module servers differ only by the module.
# Diagnostics are on: per-command server latency percentiles (INFO latencystats),
# latency-monitor events >= 1ms and slowlog entries >= 1ms.
#
#   valkey-server.sh PORT DIR [--loadmodule PATH [MODULE_ARGS...]]
#
# VALKEY_SERVER overrides the binary (default: the pinned build in .cache).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
port="${1:?usage: valkey-server.sh PORT DIR [--loadmodule PATH [ARGS...]]}"
dir="${2:?usage: valkey-server.sh PORT DIR [--loadmodule PATH [ARGS...]]}"
shift 2
server="${VALKEY_SERVER:-$here/.cache/valkey-${VALKEY_VERSION:-9.1.3}/src/valkey-server}"

mkdir -p "$dir"
exec "$server" \
  --port "$port" \
  --bind 127.0.0.1 \
  --protected-mode yes \
  --dir "$dir" \
  --logfile "$dir/valkey.log" \
  --pidfile "$dir/valkey.pid" \
  --save '' \
  --appendonly no \
  --io-threads 1 \
  --hz 10 \
  --maxmemory 0 \
  --latency-tracking yes \
  --latency-tracking-info-percentiles "50 90 99 99.9 99.99" \
  --latency-monitor-threshold 1 \
  --slowlog-log-slower-than 1000 \
  --slowlog-max-len 1024 \
  "$@"
