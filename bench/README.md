# bench

Load generation and profiling for TairHash: many keys, a few dozen short fields each,
short values and microsecond-timestamp versions.

## Overview

- `loadgen.c` — a pipelined hiredis client. Population workloads write every field of
  every key once; steady-state workloads pick a random key (and fields) per operation.
- `profile.sh` — starts a server with the module (optionally with a replica and an AOF),
  populates it, runs one workload and reports the server-side cost per operation: CPU
  time, replication-stream and AOF bytes, and the replica's CPU time and per-command cost.
  It records a CPU profile of the master, the replica or both (`perf` on Linux, `sample`
  on macOS) and the memory per key after population.

| Workload | Commands |
|---|---|
| `load` | `EXHSYNC key 0 <all fields>` per key (keys end up synced) |
| `loadset` | `EXHSET key field value ABS version` per field |
| `loadmset` | `EXHMSET` with every field of a key |
| `loadhset` | native `HSET` with an 8-byte version prefix per value: the memory baseline |
| `lww` | `EXHMSETLWW` of one random field; `--stale` % of ops carry an older version |
| `gt` | `EXHSET ... GT version` of one random field, the request shape `EXHMSETLWW` replaces; `--stale` % of ops carry an older version (error reply) |
| `mset` | `EXHMSET` of `--fields-per-write` distinct random fields |
| `getallver` | `EXHGETALLWITHVER` of a whole key |
| `syncget` | `EXHSYNCGET` of a whole key |
| `sync` | `EXHSYNCGET`, then `EXHSYNC` with a full snapshot (the read-repair path) |

## Config

`profile.sh` options (all optional):

| Option | Type | Default | Description |
|---|---|---|---|
| `--server-src` | path | `.deps/redis-7.0` | Built Redis or Valkey source tree; `tests/run.sh` creates it |
| `--module` | path | `build/scan/lib/tairhash_module.so` | Module to load; `none` for a plain server |
| `--module-arg` | `name=value` | none | Module load option for the master and the replica (repeatable) |
| `--workload` | name | `gt` | Workload to measure |
| `--preload` | name | `loadset` | Population workload run first (`none` to skip) |
| `--keys` | int | 100000 | Keys |
| `--fields` | int | 30 | Fields per key (max 256) |
| `--fields-per-write` | int | 10 | Fields per `mset` write |
| `--ops` | int | 4000000 | Operations; size it so the run outlasts the profile |
| `--pipeline` | int | 64 | Pipeline depth |
| `--stale` | percent | 10 | Share of `lww` and `gt` writes with an old version |
| `--seconds` | int | 5 | Profile length |
| `--port` | int | 27101 | Master port; the replica uses port + 1 |
| `--replica` | flag | off | Attach a replica and measure its cost of applying the stream |
| `--aof` | flag | off | Enable the master's AOF with `appendfsync no` and no automatic rewrite |
| `--profile` | name | `master` | Process to profile: `master`, `replica`, `both` or `none` |
| `--out` | path | `build/profile/<workload>` | Output directory |

`loadgen` alone: `build/loadgen -w <workload> [-h host] [-p port] [-k keys] [-f fields]
[-m fields_per_write] [-n ops] [-P pipeline] [-v value_len] [-s stale_pct] [-S seed]`.

## External Dependencies

- A built server tree (`tests/run.sh` clones and builds Redis 7.0 into `.deps/`); its
  bundled hiredis (`deps/hiredis/libhiredis.a`) is what `loadgen` links against.
- Linux: `perf` with permission to attach (`kernel.perf_event_paranoid` ≤ 1, or root).
- macOS: `sample` (ships with the OS).

## API / Interfaces

`profile.sh` prints `loadgen`'s result line (`ops_per_sec`, `errors`, ...), then:

- `master: cpu_us_per_op=... repl_bytes_per_op=... aof_bytes_per_op=...` — the master's
  CPU time (user + system, from `INFO cpu`) and the growth of `master_repl_offset` and
  `aof_current_size` over the run, divided by the operations.
- `replica: cpu_us_per_op=...` and the replica's `cmdstat_exh*` lines (calls and
  `usec_per_call` of each command it applied), with `--replica`.
- `memory: used_memory_per_key=...` after population.

Profiles and command stats are written under `<out>/master/` and `<out>/replica/`.

## Build, Run, Test

```
tests/run.sh --suite tairhash                                  # builds the server tree and the module
bench/profile.sh --workload gt                                 # single-field write path
bench/profile.sh --workload mset --replica --aof --profile both   # replication and AOF cost of a multi-field write
bench/profile.sh --workload getallver --ops 400000             # full-hash read path
bench/profile.sh --module none --preload loadhset --workload getallver --ops 1   # native memory baseline
```

`loadgen` is (re)built automatically when `loadgen.c` changes.

## Operations

Reading results:

- **Compare server CPU per operation, not throughput.** A single pipelined client rarely
  saturates the server, so `ops_per_sec` is mostly the client's ceiling.
  `cpu_us_per_op` is the server's own accounting and does not depend on the client.
- **Compare like with like.** Other load on the host inflates per-operation cost; measure
  the builds being compared back to back, interleaved (A, B, A, B), and repeat.
- **Read the profile, not just the totals.** The totals say how much a change moved the
  cost; the profile says where the cost is.
- **Mind platform artifacts on macOS.** jemalloc's thread-specific-data lookups
  (`pthread_getspecific`) inflate allocator cost relative to Linux, and on arm64 a clock
  read (`mach_absolute_time`, an `ISB`) absorbs the latency of loads still in flight.
- The flat `sample` report mixes threads; the server's idle background threads show up as
  `__psynch_cvwait` and the main thread's idle time as `kevent`.

Troubleshooting:

- `no server in ...`: run `tests/run.sh` once to build `.deps/redis-7.0`.
- `replica did not connect` / `did not catch up`: check `<out>/replica/server.log`; the
  replica must load the same module build as the master.
- `perf` exits immediately on Linux: check `kernel.perf_event_paranoid`.
- `loadgen.err` holds the first error reply of the run (expected for `gt` with `--stale`).

## Security

Benchmarks only: the servers bind to the default interface without authentication, so
run them on a development host.

## Code Map

| File | Responsibility |
|---|---|
| `loadgen.c` | Workload definitions, pipelined client, result line |
| `profile.sh` | Server and replica lifecycle, preload, profiler invocation, per-operation cost accounting |
| `README.md` | This document |
