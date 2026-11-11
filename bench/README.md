# bench

Load generation and profiling for the cache-of-a-source-of-truth workload the
last-write-wins commands target: many keys (one per entity), a few dozen fields each,
short values, microsecond-timestamp versions.

## Overview

- `lwwbench.c` — a pipelined hiredis client. Population workloads write every field of
  every key once; steady-state workloads pick a random key (and field) per operation.
- `profile.sh` — starts `redis-server` with the module, populates it, runs one workload
  and records a CPU profile of the server (`perf` on Linux, `sample` on macOS), the
  server's CPU share and memory per key.

| Workload | Commands |
|---|---|
| `load` | `EXHSYNC key 0 <all fields>` per key (keys end up synced) |
| `loadset` | `EXHSET key field value ABS version` per field (also runs on builds without EXHSYNC) |
| `loadhset` | native `HSET` with an 8-byte version prefix per value: the memory baseline |
| `lww` | `EXHMSETLWW` of one random field; `--stale` % of ops carry an older version |
| `gt` | `EXHSET ... GT version`, the request shape `EXHMSETLWW` replaces (stale = error reply) |
| `syncget` | `EXHSYNCGET` of a whole key |
| `getallver` | `EXHGETALLWITHVER` of a whole key |
| `sync` | `EXHSYNCGET`, then `EXHSYNC` with a full snapshot (the read-repair path) |

## Config

`profile.sh` options (all optional):

| Option | Type | Default | Description |
|---|---|---|---|
| `--server-src` | path | `.deps/redis-7.0` | Built server source tree; `tests/run.sh` creates it |
| `--module` | path | `build/scan/lib/tairhash_module.so` | Module to load; `none` for a plain server |
| `--workload` | name | `lww` | Workload to profile |
| `--preload` | name | `load` | Population workload run first (`none` to skip) |
| `--keys` | int | 100000 | Keys |
| `--fields` | int | 30 | Fields per key (max 256) |
| `--ops` | int | 4000000 | Operations; size it so the run outlasts the profile |
| `--pipeline` | int | 64 | Pipeline depth |
| `--stale` | percent | 10 | Share of `lww`/`gt` writes with an old version |
| `--seconds` | int | 5 | Profile length |
| `--port` | int | 27101 | Server port |
| `--out` | path | `build/profile/<workload>` | Output directory |

`lwwbench` alone: `build/lwwbench -w <workload> [-h host] [-p port] [-k keys] [-f fields]
[-n ops] [-P pipeline] [-v value_len] [-s stale_pct] [-S seed]`.

## External Dependencies

- A built server tree (`tests/run.sh` clones and builds Redis 7.0 into `.deps/`); its
  bundled hiredis (`deps/hiredis/libhiredis.a`) is what `lwwbench` links against.
- Linux: `perf` with permission to attach (`kernel.perf_event_paranoid` ≤ 1, or root).
- macOS: `sample` (ships with the OS).

## API / Interfaces

`profile.sh` prints one line with `lwwbench`'s result (`ops_per_sec`, `errors`, ...) plus
`server_cpu_pct` sampled at the start and end of the profile, then memory per key after
the preload and the profile's location.

## Build, Run, Test

```
tests/run.sh --suite tairhash_lww                  # builds the server tree and the module
bench/profile.sh --workload lww                     # ingest path
bench/profile.sh --workload sync --ops 400000       # read-repair path
bench/profile.sh --module none --preload loadhset --workload syncget --ops 1   # native memory baseline
```

`lwwbench` is (re)built automatically when `lwwbench.c` changes.

## Operations

Reading results:

- **Compare server CPU per operation, not throughput.** A single pipelined client rarely
  saturates the server (`server_cpu_pct` well under 100 means the client is the limit), so
  `ops_per_sec` is mostly the client's ceiling. Divide the command's cumulative samples by
  the operations completed during the profile instead.
- **Compare like with like.** Other load on the host inflates per-operation cost; profile
  the builds being compared back to back, interleaved (A, B, A, B).
- **Mind platform artifacts on macOS.** jemalloc's thread-specific-data lookups
  (`pthread_getspecific`) inflate allocator cost relative to Linux, and on arm64 a clock
  read (`mach_absolute_time`, an `ISB`) absorbs the latency of loads still in flight, so a
  clock read placed after cache misses looks far more expensive than it is.
- The flat `sample` report mixes threads; the server's idle background threads show up as
  `__psynch_cvwait` and the main thread's idle time as `kevent`.

Troubleshooting:

- `no server at ...`: run `tests/run.sh` once to build `.deps/redis-7.0`.
- `perf` exits immediately on Linux: check `kernel.perf_event_paranoid`.
- `bench.err` holds the first error reply of the run (expected for `gt`).

## Security

Benchmarks only: the server binds to the default interface without authentication, so run
them on a development host.

## Code Map

| File | Responsibility |
|---|---|
| `lwwbench.c` | Workload definitions, pipelined client, result line |
| `profile.sh` | Server lifecycle, preload, profiler invocation, CPU and memory sampling |
| `README.md` | This document |
