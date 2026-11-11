# TairHash for Go: driver, backends and benchmark

## Overview

This module holds four related pieces:

| Package | What it is |
|---|---|
| `tairhash` | Typed Go driver for every TairHash module command, built on [valkey-go](https://github.com/valkey-io/valkey-go). |
| `fieldhash` | The backend-neutral contract for "a hash whose fields expire independently", with three implementations: `module` (the TairHash module via the driver), `lua` (TairHash semantics in a Valkey Functions library on core types) and `native` (Valkey 9 hash-field expiration). |
| `presence` | The example use case: device presence, with one hash per user and one TTL'd field per device. It runs on any `fieldhash.Store`. |
| `cmd/thbench` | Benchmark harness that runs identical workloads against each backend, including several builds of the module side by side. It records client latency and, from the Valkey side, full INFO deltas, server-side per-command latency percentiles, the event-loop split, the slowlog and latency-monitor events, a per-second timeline, other processes' CPU in each window, and stack profiles of the server converted to pprof. It also measures expiry reclamation and memory per field, and profiles the server while it reclaims. |
| `internal/serverprof` | Converts macOS `sample` reports and Linux `perf script` output of `valkey-server` into pprof (main thread and all threads), folded stacks and a phase/component breakdown of the main thread. |

`internal/conformance` is the behavioural contract. The suite is validated against the module first, then run unchanged against `lua` and `native`. That is what makes the benchmark compare like with like: each backend has already been shown to give the same answers.

### Semantics every backend follows

These are the module's actual behaviours, confirmed against `src/tairhash.c`. A few of them differ from `CMDDOC.md`:

- A write without an expiry **clears** the field's TTL (`EXHSET`, `EXHINCRBY`). Use `tairhash.KeepTTL()` to keep it.
- TTLs are measured on the **server** clock. A field is expired once `now_ms >= expire_at_ms`. `PX 0`/`PXAT 0` mean "expire immediately".
- Each write bumps the field's version, starting at 1. `VER v` must match the current version (`v == 0` disables the check, and absent fields always match). `ABS v` forces the version, and `GT v` requires `v` to be greater than the current version and then stores it.
- `NX`/`XX` are evaluated before the version check.
- Expired fields are invisible to every read and are deleted when a command touches them. Every write first reclaims up to 3 due fields from the key's expiry index ("passive expire"). Fields nobody touches are reclaimed by active expiration.
- `EXHPTTL` returns `-2` for a missing key and **`-3`** for a missing or expired field (CMDDOC says `-2` for both). `tairhash.ParsePTTL` handles both.
- `EXHMSETWITHOPTS` takes TTLs in relative **seconds**, and `0` means *expire immediately*, not "no TTL". The driver requires whole seconds ≥ 1s.

### Known module defect (upstream `9ee21d6` and `ci/valkey` `0073ca1`; removed on `fix/replicate-field-state` `14fc2b1`)

`EXHINCRBY` and `EXHINCRBYFLOAT` with an expiry replicate `PXAT (milliseconds + now)` (`src/tairhash.c:1536` and `:1780` on `9ee21d6`). `milliseconds` is already absolute, so replicas get a deadline about 56 years out. Reproduced on `e64cd44`: primary `EXHPTTL` 59493 ms, replica 1794387143522 ms. `EXHSET` replicates correctly. `14fc2b1` replicates each write's resulting field state instead, and the expression is gone from it and from every later commit of that stack. On builds that still have it, counters with TTLs never expire on replicas or after a failover.

## Config

### `thbench run` / `thbench reclaim` (shared flags)

| Flag | Type | Default | Description |
|---|---|---|---|
| `-addr` | string | `127.0.0.1:6379` | Server used by backends listed without `@addr`. |
| `-backends` | list | `module,lua,native` | `[name=]type[@host:port]`, where type is `module`, `lua` or `native`. The name defaults to the type, must be unique and must match `[a-z0-9][a-z0-9._+-]*`; it is what results, the report and artifact paths call the backend, so builds of one type are told apart by name (`develop=module@127.0.0.1:26380,lazy=module@127.0.0.1:26381`). Give each module its own server: its active-expire timer SCANs every database once a second and would otherwise run against the other backends' data. |
| `-db` | int | `9` | Database the bench owns. It is **flushed** before and after every run, so idle servers hold no data for module timers to scan while other backends run. |
| `-flush` | bool | `false` | Allow flushing `-db` when it already holds keys. Without it the bench refuses to start on a non-empty database. |
| `-keys` | int | `10000` | Number of hashes. |
| `-fields` | int | `10` | Fields per hash. |
| `-value-size` | int | `64` | Bytes per value. |
| `-out` | path | `results.jsonl` | JSON-lines file that results are appended and fsynced to after each run. |
| `-label` | string | `""` | Free-form label stored on each result (for example the module revision and load args). The report renders each label separately and never pools results across labels. Artifacts go under a path-safe directory derived from it (see [Artifacts](#artifacts)). |
| `-placement` | string | `""` | How the client and servers are bound to CPUs, stored in `host.placement` and shown in the report environment. `scripts/bench.sh` sets it. |
| `-artifacts-dir` | path | `""` | Per-run raw server captures go here (see [Artifacts](#artifacts)). Required by `-profile`. |
| `-pipeline-multiplex` | int | `2` | valkey-go `PipelineMultiplex`: 2ⁿ auto-pipelined connections per backend. |
| `-conn-write-timeout` | duration | `10s` | valkey-go `ConnWriteTimeout`. |

### `thbench run`

| Flag | Type | Default | Description |
|---|---|---|---|
| `-workloads` | list | `set-ttl,get,getall,incr-ttl,mixed,presence,cas` | See [Workloads](#workloads). `cas` runs only on backends with versions. |
| `-concurrency` | list | `1,16,128` | Client goroutines per run. |
| `-duration` | duration | `10s` | Measured window. |
| `-warmup` | duration | `3s` | Unmeasured warmup. |
| `-repeat` | int | `3` | Runs per cell. The report uses medians. |
| `-ttl` | duration | `1h` | Lease on written fields. It must outlast the run, so steady-state workloads measure access cost rather than expiry. |
| `-seed` | uint | `1` | Base seed for key selection (deterministic per worker). |
| `-timeline-every` | duration | `1s` | Server timeline sampling period during measured windows. `0` disables it. |
| `-profile` | bool | `false` | After the measured repeats, run every cell once more with the client CPU profiler and the server profiler. Profilers perturb timing, so the report uses profiled runs only for their profiles and never for their numbers. |
| `-profile-concurrency` | list | every `-concurrency` level | Concurrency levels the profiled pass covers. Each must be one of `-concurrency`. |
| `-server-profiler` | string | `auto` | `auto` (sample on macOS, perf on Linux when installed), `sample`, `perf` or `none`. |
| `-server-profiler-cmd` | string | `""` | Overrides the profiler command. The template is split on whitespace first, then `{pid}`, `{secs}` and `{out}` are substituted inside each argument, so an output path with spaces stays one argument. Defaults: `sample {pid} {secs} 1 -mayDie -file {out}`; `perf record -F 999 -g -p {pid} -o {out} -- sleep {secs}`. |

### `thbench reclaim`

| Flag | Type | Default | Description |
|---|---|---|---|
| `-lease` | duration | `30s` | Time from load start to the shared expiry deadline. It must exceed the load time. |
| `-timeout` | duration | `5m` | How long to wait for reclamation after the deadline before recording "not reclaimed". |
| `-poll` | duration | `100ms` | `DBSIZE`/`used_memory` sampling interval. |
| `-sweep-every` | duration | `100ms` | Period of the client-driven sweeper (`lua` only). |
| `-sweep-budget` | int | `10000` | Fields the sweeper may reclaim per period. |
| `-idle-window` | duration | `3s` | Measures the empty server's background busy rate (cron, module timers) before loading. That rate times the reclaim window is subtracted from the reclaim work, so a slow reclaimer is not charged for idle event-loop time. `0` disables the subtraction. |
| `-memory-sample-keys` | int | `200` | Logical keys measured with exact `MEMORY USAGE` after loading. Every physical key is summed, so for `lua` that is the data hash plus its index. |
| `-profile` | bool | `false` | After the measured reclaims, reclaim on every backend once more with the client CPU profiler and a server profile, and stop polling when the profile ends. Needs `-artifacts-dir`. The report shows profiled reclaims only as profiles. |
| `-profile-window` | duration | `10s` | Server profile length for `-profile`, from the opening of the reclaim window (500 ms before the deadline). A slow reclaimer is caught in steady state; a fast one is captured whole, with the idle remainder excluded from the percentages. |
| `-server-profiler`, `-server-profiler-cmd` | string | `auto`, `""` | As for `thbench run`. |

### `thbench report`

| Flag | Type | Default | Description |
|---|---|---|---|
| `-reference` | string | `""` | Backend every other is compared against, in the summary, the paired-ratio columns and the reclaim B/field ratio. Empty, or not present in a label, picks `module` when the label has it, else the label's first backend. |

### `thbench profile`

| Flag | Type | Default | Description |
|---|---|---|---|
| `-format` | string | `sample` | `sample` (macOS report) or `perf` (Linux `perf.data`, decoded with `perf script`). |
| `-pid` | int | `0` | Server pid. `perf` needs it to identify the main thread. |
| `-period` | duration | `1ms` | Sampling period of the capture. |
| `-out` | path | RAW without extension | Output prefix for `.main.pprof`, `.all.pprof`, `.folded`, `.summary.json`. |
| `-top` | int | `25` | Functions listed in the summary. |

### `presence-demo`

| Flag | Type | Default | Description |
|---|---|---|---|
| `-addr` | string | `127.0.0.1:6379` | Server. |
| `-backend` | string | `module` | `module`, `lua` or `native`. |
| `-user` | string | `alice` | Simulated user. |
| `-devices` | int | `3` | Simulated devices. |
| `-ttl` | duration | `3s` | Presence lease. Heartbeats run every TTL/3. |
| `-crash-after` | duration | `4s` | When `device-0` stops heartbeating. |
| `-run` | duration | `12s` | Run time before clean disconnects. |
| `-shutdown-timeout` | duration | `5s` | Upper bound on the exit-time disconnects. |

### `presence.Registry` options

| Option | Type | Default | Description |
|---|---|---|---|
| `WithTTL` | `time.Duration` | `30s` | How long a device stays online after a heartbeat (≥ 1ms). |
| `WithKeyPrefix` | string | `presence:` | Prefix of each user's hash key. |

### Environment and make variables

| Name | Default | Used by | Description |
|---|---|---|---|
| `VALKEY_ADDR` | unset | tests | Integration tests skip unless set. Each test package owns one database (10–14, see `internal/testserver`) and flushes it. |
| `VALKEY_VERSION` | `9.1.3` | make, scripts | Pinned Valkey tag built into `.cache/`. |
| `VALKEY_MALLOC` | `jemalloc` | make | Allocator Valkey is built with, on every platform. Valkey's own default is libc malloc on macOS, where every allocation and free also pays a `malloc_size` lookup. That falls hardest on the module and Lua, whose operations allocate the most. Production Valkey runs jemalloc. Changing it triggers a full rebuild. `server.mem_allocator` in each result records which one ran. |
| `VALKEY_SERVER` | `.cache/valkey-$VALKEY_VERSION/src/valkey-server` | `scripts/valkey-server.sh` | Server binary override. |
| `MODULE_SRC` | `..` | make | TairHash tree the module is built from. It must be the top of a git checkout, because the build records that checkout's commit. Each tree gets its own CMake build directory inside the build's directory. |
| `MODULE_NAME` | `local` | make | Name the build is kept under: `.cache/modules/<name>-<mode>/` holds `tairhash_module.so`, `source-revision` (commit, then `git status --porcelain`), `compile_commands.json` (the exact compiler invocations) and the CMake build trees. |
| `MODULE_MODE` | `scan` | make | `scan`, `sort` or `slab` active-expire algorithm. |
| `MODULE_JOBS` | `4` | make | Parallel compile jobs for `make module`. |
| `MODULE_SO` | `.cache/modules/local-scan/tairhash_module.so` | `scripts/bench.sh` | Module to load when `MODULES` is unset. |
| `MODULES` | `module=$MODULE_SO` | `scripts/bench.sh` | Space-separated `name=path.so` builds. Each gets its own server, on `MODULE_PORT`+i, and is benchmarked as backend `name`. |
| `PLAIN_BACKENDS` | `lua native` | `scripts/bench.sh` | Backends run on the plain server. Empty runs no plain server. |
| `MODULE_ARGS` | `""` | `scripts/bench.sh` | Module load args for every build, for example `active_expire_period 100 active_expire_keys_per_loop 10000`. |
| `LABEL` | one module: `tairhash@<rev>[+dirty]`; several: `modules <names>`; `ladder.sh`: `ladder`; each plus ` MODULE_ARGS` | `scripts/bench.sh`, `scripts/ladder.sh` | Label passed to both `run` and `reclaim`. `<rev>` is the first 7 characters of the commit in `source-revision` next to the module, with `+dirty` when that file lists uncommitted changes. |
| `REFERENCE` | `bench.sh`: unset (module, else the first backend); `ladder.sh`: the first name | `scripts/bench.sh`, `scripts/ladder.sh` | `thbench report -reference`. |
| `CLIENT_QOS` | `utility` | `scripts/bench.sh` (macOS) | QoS clamp the client runs under (`taskpolicy -c`). Empty disables it. `background` is too slow: the client can no longer saturate the server. See [Troubleshooting](#troubleshooting). |
| `CLIENT_CPUS` / `SERVER_CPUS` | unset | `scripts/bench.sh` (Linux) | `taskset -c` CPU lists for the client and for both servers. Give the server cores of its own. |
| `PLAIN_PORT` / `MODULE_PORT` | `26379` / `26380` | `scripts/bench.sh` | Port of the plain server, and of the first module server; build i listens on `MODULE_PORT`+i. |
| `RECLAIM_ARGS` | `""` | `scripts/bench.sh` | Extra `thbench reclaim` flags. |
| `SKIP_LOAD` | unset | `scripts/bench.sh` | Set to skip the load sweep, for a reclaim-only pass into an existing `OUT` (see [Build / Run / Test](#build--run--test)). |
| `SKIP_RECLAIM` | unset | `scripts/bench.sh` | Set to skip the reclaim scenario. |
| `SKIP_PROFILE` | unset | `scripts/bench.sh` | Set to skip the profiled pass. |
| `TAIRHASH_REPO` | the checkout this module lives in | `scripts/ladder.sh` | Repository the ladder's refs resolve in. Each revision is cloned from it with `--shared` into `.cache/src/<name>-<commit>`. |
| `WORKLOADS` | every workload | `scripts/ladder.sh` | Workloads, comma- or space-separated; each is one `bench.sh` chunk with its own lock hold. |
| `CONCURRENCY` | `16,128` | `scripts/ladder.sh` | `-concurrency` of every chunk. |
| `PROFILE_CONCURRENCY` | last of `CONCURRENCY` | `scripts/ladder.sh` | `-profile-concurrency` of every chunk. |
| `REPEAT` | `3` | `scripts/ladder.sh` | `-repeat` of every chunk. |
| `RUN_ARGS` | `""` | `scripts/ladder.sh` | Extra `thbench run` flags for every chunk. |
| `RECLAIM_ARGS` (ladder) | `-profile` | `scripts/ladder.sh` | Reclaim flags of the reclaim chunk. |
| `CONFORMANCE_PORT` | `26990` | `scripts/ladder.sh` | Port of the throwaway server each build's conformance run uses. |
| `SKIP_CONFORMANCE` | unset | `scripts/ladder.sh` | Set to benchmark builds without the conformance gate. |
| `TEST_PORT` | `26999` | `make test-integration` | Port of the throwaway test server. |
| `OUT` | `bench/results/<UTC timestamp>` | `make bench`, `make report` | Output directory. |
| `BENCH_FLAGS` | `""` | `make bench` | Extra `thbench run` flags. |

## External Dependencies

| Dependency | Version | Why |
|---|---|---|
| Go | 1.27 | Toolchain. |
| `github.com/valkey-io/valkey-go` | v1.0.79 (latest at time of writing) | Client: RESP3, auto-pipelining, typed builders. |
| `github.com/HdrHistogram/hdrhistogram-go` | v1.4.0 | Latency histograms (1µs–60s, 3 significant digits). |
| `github.com/stretchr/testify` | v1.12.1 | Test assertions. |
| Valkey | 9.1.3, built from the tagged source | Server. `native` needs ≥ 9.0 (`HSETEX`). `lua` needs Functions (≥ 7). |
| TairHash module | this repository | `module` backend. |
| CMake ≥ 3.10, a C compiler | — | Module build. |
| `github.com/google/pprof` | v0.0.0-20261111065629 (upstream publishes no tags) | Writes server profiles in pprof format. |
| `sample` (macOS) / `perf` (Linux) | — | Server stack sampling for the profiled pass. The Valkey build already uses `-fno-omit-frame-pointer -g`, and `make module` adds `-fno-omit-frame-pointer` to the module, so stacks stay walkable through module frames. |
| `gate-lock` | optional for `bench.sh`, required by `ladder.sh` | `scripts/bench.sh` re-executes itself under `gate-lock run-exclusive` when it is on `PATH`. `scripts/ladder.sh` builds under `gate-lock run` (4 cores) and runs conformance under `gate-lock run-focused`. |
| `taskpolicy` (macOS) / `taskset` (Linux, util-linux) | — | Client and server CPU placement in `scripts/bench.sh`. |
| `ps` (macOS) / procfs (Linux) | — | Per-process CPU times sampled around every window for `interference`. |
| git | — | `make module` records the source commit; `scripts/ladder.sh` clones each revision. |

## API / Interfaces

### Driver (`tairhash`)

```go
c := tairhash.New(vk) // vk is a valkey.Client the caller owns
res, err := c.Set(ctx, "k", "f", "v", tairhash.SetArgs{
    Expiry:    tairhash.TTL(30 * time.Second), // NoExpiry() (zero value) | KeepTTL() | TTL(d) | ExpireAt(t)
    Condition: tairhash.IfNotExists,           // Always | IfNotExists (NX) | IfExists (XX)
    Version:   tairhash.MatchVersion(3),       // zero value | MatchVersion (VER) | ForceVersion (ABS) | AdvanceVersion (GT)
})
// res is tairhash.Created, Updated or NotSet. A failed version check returns ErrStaleVersion.
```

| Method | Command |
|---|---|
| `Set`, `SetNX`, `MSet`, `MSetWithOptions` | `EXHSET`, `EXHSETNX`, `EXHMSET`, `EXHMSETWITHOPTS` |
| `IncrBy`, `IncrByFloat` | `EXHINCRBY`, `EXHINCRBYFLOAT` (with `MIN`/`MAX`) |
| `Expire`, `Persist`, `PTTL` | `EXHPEXPIRE`/`EXHPEXPIREAT`, `EXHPERSIST`, `EXHPTTL` |
| `Version`, `SetVersion` | `EXHVER`, `EXHSETVER` |
| `Get`, `GetWithVersion`, `MGet`, `MGetWithVersion` | `EXHGET`, `EXHGETWITHVER`, `EXHMGET`, `EXHMGETWITHVER` |
| `GetAll`, `GetAllWithVersion`, `Keys`, `Vals`, `Scan` | `EXHGETALL`, `EXHGETALLWITHVER`, `EXHKEYS`, `EXHVALS`, `EXHSCAN` |
| `Del`, `DelWithVersion` | `EXHDEL`, `EXHDELWITHVER` |
| `Len`, `Exists`, `StrLen` | `EXHLEN [NOEXP]`, `EXHEXISTS`, `EXHSTRLEN` |
| `Ping` | Verifies the module is loaded (`ErrModuleNotLoaded` otherwise). |

The driver validates arguments before sending (`ErrInvalidArgument`) and maps server replies to sentinels for `errors.Is`: `ErrStaleVersion`, `ErrOverflow`, `ErrNotInteger`, `ErrNotFloat`, `ErrWrongType`, `ErrModuleNotLoaded`. Every error carries the command and key. Read commands are marked read-only, so valkey-go may retry them after network errors.

### `fieldhash` contract

```go
type Store interface {
    Set(ctx, key, field, value string, exp tairhash.Expiry, cond tairhash.Condition) (bool, error)
    Get(ctx, key, field string) (string, bool, error)
    GetAll(ctx, key string) (map[string]string, error)
    Del(ctx, key string, fields ...string) (int64, error)
    IncrBy(ctx, key, field string, delta int64, exp tairhash.Expiry) (int64, error)
    PTTL(ctx, key, field string) (tairhash.FieldTTL, error)
}
type VersionedStore interface { Store; SetWithArgs(...); GetWithVersion(...) } // module, lua
type Sweeper interface { Sweep(ctx, maxFields int) (int64, error) }           // lua
```

| Backend | Commands | Versions | Who reclaims expired fields |
|---|---|---|---|
| `module` | `EXH*` | yes | Module timer (SCAN mode: every 1s, ≤ 1000 fields per loop by default) plus passive expiry on writes. |
| `lua` | `FCALL fh_*` (library `fieldhash`, `fieldhash/lua/fieldhash.lua`) | yes, exact to 2⁵³ | Passive expiry on writes plus `Sweep`, which SCANs for expiry indexes. Run it periodically. |
| `native` | `HSETEX`, `HGET`, `HGETALL`, `HDEL`, `HINCRBY`+`HPEXPIRE` in `MULTI`, `HPTTL` | no | Valkey's active expire cycle. |

Lua storage layout: logical key `K` is stored as two keys under one hash tag, so each call stays in one cluster slot:

- `{K}`: a hash `field → struct.pack('<dd', version, expire_at_ms) .. value`
- `{K}:exp`: a zset `field → expire_at_ms`

The client reloads the library automatically if the server loses it (`FUNCTION FLUSH`, or a restart without persistence).

### Workloads

| Name | Operation |
|---|---|
| `set-ttl` | Overwrite an existing field and renew its TTL. |
| `get` | Read a live field that has a TTL. |
| `getall` | Read all fields of one hash. |
| `incr-ttl` | Increment a counter and renew its TTL. |
| `mixed` | 80% `get`, 20% `set-ttl`. |
| `presence` | `presence.Registry`: 90% heartbeat, 9% list devices, 1% disconnect. |
| `cas` | Read value and version, then write back with `VER`; retry on conflict (module and lua only). |
| `reclaim` (separate command) | Load all fields with one shared deadline, then measure time and server cost until they are gone without any reads. Also reports bytes per field. |

## Build / Run / Test

```sh
make valkey                    # pinned Valkey into .cache/ (jemalloc)
make module                    # TairHash module (MODULE_MODE=scan|sort|slab, MODULE_SRC=.., MODULE_NAME=local)
make test                      # unit tests, no server
make test-integration          # throwaway server with the module, every test with -race
make lint                      # gofmt, go vet, shellcheck

go run ./cmd/presence-demo -addr 127.0.0.1:6379 -backend module

make bench OUT=bench/results/run1 BENCH_FLAGS='-repeat 3 -duration 10s'
make report OUT=bench/results/run1

scripts/ladder.sh bench/results/ladder1 develop=develop lazy=perf/lazy-expire-index tip=perf/embedded-field-table
```

`scripts/bench.sh` starts otherwise identical servers from `scripts/valkey-server.sh`: `plain` for `lua` and `native`, and one per module build with TairHash loaded. Both use `io-threads 1`, `hz 10`, have persistence off, and have diagnostics on: `latency-tracking-info-percentiles "50 90 99 99.9 99.99"`, `latency-monitor-threshold 1` (ms) and `slowlog-log-slower-than 1000` (µs). The script runs the measured repeats, one profiled pass and the reclaim scenario, and writes the following to `OUT`:

- `results.jsonl`
- `report.md`
- `runs/<label dir>/<run>/`
- `run.log` and `reclaim.log`
- the server logs
- `host-load.txt`: the host load before and after each pass, the label, and the CPU placement (appended across passes)

The client runs under the placement described by `CLIENT_QOS` (macOS) or `CLIENT_CPUS`/`SERVER_CPUS` (Linux). Each result records it. If a phase fails, the script skips the remaining phases, still renders `report.md` from the results recorded so far, and exits with the failing phase's status.

To measure tuned module expiry next to the defaults, run a second reclaim-only pass into the same `OUT`. The pass gets its own label from `MODULE_ARGS`, so the report keeps the two apart:

```sh
SKIP_LOAD=1 PLAIN_BACKENDS= MODULE_ARGS='active_expire_period 100 active_expire_keys_per_loop 10000' \
  MODULE_SO=$PWD/.cache/modules/local-scan/tairhash_module.so scripts/bench.sh bench/results/run1
```

`scripts/ladder.sh OUT name=ref...` compares module revisions with each other and with `lua` and `native`:

1. It clones each ref into `.cache/src/` and builds it as `.cache/modules/<name>-scan/`.
2. It runs the integration suites (`tairhash`, `fieldhash/{module,lua,native}`, `presence`, with `-race`) against a throwaway server with that build loaded. Any failure stops the ladder before benchmarking, because a build that answers differently is not comparable.
3. It runs `bench.sh` once per workload with every build on its own server, then once for reclaim with `-profile`. Backends are interleaved within each cell, so the paired ratios against the reference (the first name) cancel drift between repeats.

`OUT/ladder.txt` lists each name's ref, commit, subject, `-march` flags from `compile_commands.json` and conformance verdict, plus chunk start times. Each chunk takes the host lock separately and appends to the same results, so a failed chunk can be rerun on its own with `WORKLOADS=<name> SKIP_RECLAIM=1` into a fresh directory, and both rendered together with `thbench report -reference <name> A/results.jsonl B/results.jsonl`.

Re-analyse any server capture with `thbench profile RAW`, and open profiles with `go tool pprof -http=: OUT/runs/<label dir>/<run>/server.main.pprof`.

## Operations

### Reading the results

Each run brackets its measured window with `CONFIG RESETSTAT`, `SLOWLOG RESET` and `LATENCY RESET`, followed by a full `INFO everything` + `INFO debug` snapshot. It takes a second snapshot at the end. Everything below comes from the server unless marked client.

| Metric (`server_metrics.*` in `results.jsonl`) | Meaning |
|---|---|
| `ops_per_sec`, `latency.p50/p99/p999_us` (client) | Client-observed throughput and latency, including the loopback round trip and client CPU. |
| Report `vs <reference> (paired)` | For each repeat, the backend's value divided by the reference backend's in the same repeat, then the median of those ratios, with their spread. A cell's backends run back to back within a repeat, so pairing cancels host-load drift between repeats. A ratio of medians would keep that drift. The reference is `thbench report -reference`. |
| Report summary | Per label and concurrency, a backend × workload matrix of median busy µs/op and ops/s, each with its paired ratio against the reference. `†` marks a cell where a measured run saw other processes use more than 1 core. |
| `interference.foreign_cores`, `foreign_cpu_sec`, `top` | CPU, user plus system, used during the window by every process except the client, its children (`ps`, the profiler) and the measured server, sampled from `ps`/procfs just outside the server snapshots. The other benchmark servers count. Processes that start and exit inside the window are missed, so it is a lower bound. `top` names the five largest consumers; the report environment totals them over all windows. |
| `busy_us_per_op` | (Δ`eventloop_duration_sum` − `harness_sec`) ÷ ops: main-thread time per operation, including parsing, replies, timers and cron. Every command runs on that thread, so this sets peak throughput and is the primary comparison metric. `eventloop_duration_sum` covers each event-loop iteration from wakeup to the next sleep. `used_active_time_main_thread` is not used because it only counts iterations that did client I/O, so module timers and cron-driven expiry, which run in idle iterations, would vanish from expiry measurements. |
| `busy_main_sec`, `harness_sec` | Δ`eventloop_duration_sum` in seconds, and the commandstats execution time of the bench's own commands (`INFO`, `DBSIZE`, `PING`, resets, `MEMORY`, `EXHEXPIREINFO`) in the same window. Their parse and reply overhead, a few µs each, is not separable and stays in. |
| `cpu_us_per_op` | Δ(`used_cpu_user` + `used_cpu_sys`) ÷ ops, across all threads. |
| `utilization`, `timeline.utilization_min/max` | Δ`eventloop_duration_sum` ÷ wall time, for the window and per second. Below ~0.9 the client or the round trip limited the run. A wide min–max range means the window was not steady. |
| `eventloop.us_per_op`, `cmd_us_per_op`, `noncmd_us_per_op` | `eventloop_duration_sum` and `eventloop_duration_cmd_sum` per op. This splits server time into command execution and everything else (read, parse, reply, before/after-sleep, timers). |
| `eventloop.ops_per_cycle` | Operations per event-loop iteration: the batching factor that amortises syscalls and wakeups. |
| `io.reads_per_op`, `io.writes_per_op`, `io.in_bytes_per_op`, `io.out_bytes_per_op` | Socket reads/writes and bytes per operation (FCALL's argument encoding shows up in `in_bytes`). |
| `commands_per_op`, `per_command[*]` | Every executed command, including those issued by scripts and module timers (the module's active expiry appears as `scan`). Per command: calls/op, µs/call, and server-side `p50/p90/p99/p99.9/p99.99` from `INFO latencystats`. |
| `errors`, `error_replies` | `INFO errorstats` and `total_error_replies` in the window. |
| `expired_fields`, `expired_keys`, `module_active/passive_expired_fields` | Core (native) and module expiry counters. The module ones come from `EXHEXPIREINFO`. |
| `memory.*` | `used_memory`, dataset, overhead, Lua VM (`vm_functions`, `vm_eval`), RSS and fragmentation at the end, plus deltas. |
| `slowlog`, `latency_events` | SLOWLOG entries (≥ 1 ms) and latency-monitor events (≥ 1 ms) in the window. |
| `server_profile.summary` (profiled pass) | Main-thread phase and component breakdown, top self/cumulative functions, and unclassified leaves. |
| Reclaim: `bytes_per_field`, `dataset_bytes_per_field`, `memory_usage_per_field` | `used_memory` growth, `used_memory_dataset` growth and exact `MEMORY USAGE` per field. `used_memory` is the cross-backend number of record: `MEMORY USAGE` of a module key reports whatever the module's `mem_usage` callback returns. |
| Reclaim: `idle_busy_per_sec`, `busy_us_per_field` | The empty server's background busy rate, measured over `-idle-window` before loading, and the reclaim work per field: Δ`eventloop_duration_sum` in the window minus harness commands minus `idle_busy_per_sec` × window, divided by fields reclaimed. `server_metrics.busy_us_per_op` is the same without the idle subtraction. |
| Reclaim: `server_metrics.busy_us_per_op` | Main-thread cost per reclaimed field, including module timers, core active expiry and client-driven sweeping for `lua`, net of the harness's polling. The window opens 500 ms before the deadline, so reclamation that starts the moment the deadline passes is inside it. The `lua` sweeper starts at the deadline itself. |

### Artifacts

`-artifacts-dir` gets one directory per run at `<label dir>/<run>`. The run is `<backend>-<workload>-c<conc>-r<repeat>`, `...-profile`, `reclaim-<backend>` or `reclaim-<backend>-profile`, and is also stored as `run` in each result. Each result also records `backend_type` and, for module servers, `server.tairhash_revision`, read from `source-revision` next to the loaded `.so`. The label directory is the label with every run of characters outside `[A-Za-z0-9._@+-]` collapsed to `-`. When that changes the label, an 8-hex-digit hash of the original is appended so distinct labels never share a directory. Without a label, runs sit directly under `-artifacts-dir`.

| File | Content |
|---|---|
| `info.before.txt`, `info.after.txt` | Full `INFO everything` + `INFO debug` bracketing the window (reclaim: baseline, loaded, deadline (taken 500 ms before it), done). |
| `config.txt` | `CONFIG GET *`. |
| `memory-stats.txt` | `MEMORY STATS` at the end of the window. |
| `slowlog.json`, `latency.json` | SLOWLOG entries; `LATENCY LATEST` and `LATENCY HISTORY` per event. |
| `timeline.jsonl` | Per-second utilization, CPU, command rate, loop cycles, network bytes, expired fields, memory, clients. |
| `module-expire.*.txt` | `EXHEXPIREINFO` (module server only). |
| `result.json` | The run's full result record. |
| `client.pprof` | Go client CPU profile (profiled pass). |
| `server.sample.txt` / `server.perf.data` | Raw server capture (profiled pass). |
| `server.main.pprof`, `server.all.pprof`, `server.folded`, `server.summary.json` | Converted server profile: main thread, all threads, flame-graph input, phase/component summary. |

### Server profile breakdown

`internal/serverprof` attributes each main-thread sample to a **phase**: the first match in priority order of `processTimeEvents` (timers), `call` (command), client writes (write), `activeExpireCycle` (expire), `readQueryFromClient` (read+parse), `aeApiPoll` (poll), and before/after-sleep. Timers outrank command because module timers execute commands through `RedisModule_Call`.

Each sample also gets a **component**, chosen by the leaf-most matching frame: alloc, memcpy, syscall, module (frames in `tairhash_module.so`), module-api (`VM_*`; Valkey 9's Lua engine is itself a module, so Lua runs show it too), lua, hash, zset, keyspace, reply, string/object, parse, dispatch, prefetch, clock (libc `localtime_r`/TZ lookups around each loop iteration), and eventloop.

macOS `sample` is wall-clock, so main-thread samples blocked in `kevent` count as idle and are excluded from percentages. Linux `perf` samples on-CPU only. The rules are heuristics: `unclassified` in each summary lists any leaf functions that matched nothing.

### Troubleshooting

- **`db N holds K keys; pass -flush`**: the bench database is not empty. Point `-db` at a scratch database, or pass `-flush` if erasing it is fine.
- **`backend module: tairhash: module not loaded`**: the server at that address lacks the module. Check `MODULE LIST`, and run module benchmarks with `module@host:port`.
- **`backend native: server lacks HSETEX`**: the server is older than Valkey 9.0.
- **`name given twice`**: two `-backends` entries resolve to the same name, usually two modules without `name=`. Name each build.
- **`ladder.sh`: `conformance failed for: <name>`**: that build answers differently from the contract. Read `OUT/conformance-<name>.log`. Fix the build, or drop it from the ladder; do not benchmark it with `SKIP_CONFORMANCE=1` and compare it as if it were equivalent.
- **`ladder.sh`: `<name>: build failed`**: read `OUT/build-<name>.log`. Revisions older than the CMake 4 fix configure only because `make module` passes `CMAKE_POLICY_VERSION_MINIMUM=3.5`.
- **`MODULE_SRC=... is not the top of a git checkout`**: point `MODULE_SRC` at a checkout's root. A source tree nested in another repository would be recorded under that repository's commit.
- **`†` in the summary, or a high `foreign cores` column**: another process took CPU during that cell. The report environment names the largest consumers. Compare with the paired ratio, and rerun the cell's workload if the marked runs disagree with their repeats.
- **`load took Xs, past the lease`** or **`reclaim window opened ... after the deadline`** (reclaim): loading, `MEMORY USAGE` sampling or the window-opening snapshot outran the shared deadline. Raise `-lease`, or shrink `-keys`/`-fields`/`-memory-sample-keys`.
- **reclaim reports `no (N keys left)` for `module`**: expected with SCAN-mode defaults, which reclaim ≤ 1000 fields per second per database. Check `server.tairhash_active_expire` in the result, then rerun with `MODULE_ARGS='active_expire_period 100 active_expire_keys_per_loop 10000'` to measure a tuned configuration.
- **Large run-to-run spread, or throughput that falls with `load_avg_1m_start_end`**: the server's main thread is competing for CPU.
  - **Apple silicon**: an M4 has 4 performance and 6 efficiency cores. Once load exceeds the performance-core count, the scheduler can move `valkey-server` onto an efficiency core, and its cost per op rises by tens of percent. Check `host.placement` in the result: `client: taskpolicy -c utility` keeps the load generator mostly off the performance cores. Other processes still compete, so check `host-load.txt` and the per-repeat load in each result. Compare backends with the paired `vs module` ratio, not absolute numbers.
  - **Linux**: set `SERVER_CPUS` and `CLIENT_CPUS` to disjoint cores.
  - **Either platform**: `scripts/bench.sh` takes `gate-lock run-exclusive` when it is available.
- **`server profiler: sample: ... exit status`** (macOS): `sample` must be able to attach to `valkey-server`. That works for a binary you built and run as the same user. A hardened or SIP-protected binary refuses. Use `-server-profiler none` to skip it.
- **`server profiler: perf: ... Permission denied`** (Linux): lower `kernel.perf_event_paranoid` (for example `sysctl kernel.perf_event_paranoid=1`) or run with `CAP_PERFMON`. Install `perf` matching the kernel.
- **Profile summary lists many `unclassified` leaves**: a new Valkey version renamed hot functions. Extend `componentRules` in `internal/serverprof/classify.go`, then re-run `thbench profile` on the saved raw capture.
- **`errors` column non-zero**: check `errors` in `result.json` (from `INFO errorstats`). The harness itself issues no failing commands, so any error reply is a real backend error.
- **Lua `FCALL` errors `Function not found`**: the client reloads the library and retries once. Persistent failures mean `FUNCTION LOAD` itself fails, so check the server log.

## Security

- The bench **flushes** its database. It refuses to start on a non-empty database unless `-flush` is passed, and it never touches database 0 unless told to.
- Servers started by the scripts bind `127.0.0.1` with protected mode on and no persistence. They are not meant to be exposed.
- No credentials are handled. To add AUTH or TLS, extend `valkey.ClientOption` in the commands; secrets must come from the environment, not flags that show up in process listings.
- Error messages include keys and fields, so treat them as data when keys contain user identifiers.

## Code Map

```
go/
├── tairhash/              driver: args.go (options, validation), client.go (commands), errors.go (reply → sentinel)
├── fieldhash/
│   ├── fieldhash.go       Store / VersionedStore / Sweeper contracts
│   ├── module/            adapter over the driver
│   ├── lua/               fieldhash.lua (Valkey Functions library) + Go client and sweeper
│   └── native/            Valkey 9 hash-field expiration
├── presence/              use case: device presence registry
├── internal/
│   ├── conformance/       shared behavioural suite every backend passes
│   ├── testserver/        integration-test connection and database allocation
│   ├── serverprof/        sample/perf → pprof, folded stacks, main-thread phase breakdown
│   └── bench/             workloads, backend specs, runner, server snapshots + metrics, timeline, interference, artifacts, reclaim, report
├── cmd/
│   ├── thbench/           benchmark CLI (run / reclaim / report / profile)
│   └── presence-demo/     runnable use-case demo
├── scripts/               valkey-server.sh (canonical server flags), bench.sh (orchestration), ladder.sh (revision ladder)
└── Makefile
```
