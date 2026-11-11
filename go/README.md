# TairHash for Go: driver, backends and benchmark

## Overview

This module holds four related pieces:

| Package | What it is |
|---|---|
| `tairhash` | Typed Go driver for every TairHash module command, built on [valkey-go](https://github.com/valkey-io/valkey-go). |
| `fieldhash` | The backend-neutral contract for "a hash whose fields expire independently", with three implementations: `module` (the TairHash module via the driver), `lua` (TairHash semantics in a Valkey Functions library on core types) and `native` (Valkey 9 hash-field expiration). |
| `presence` | The example use case: device presence, with one hash per user and one TTL'd field per device. It runs on any `fieldhash.Store`. |
| `cmd/thbench` | Benchmark harness that runs identical workloads against each backend. It records client latency and, from the Valkey side, full INFO deltas, server-side per-command latency percentiles, the event-loop split, the slowlog and latency-monitor events, a per-second timeline, and stack profiles of the server converted to pprof. It also measures expiry reclamation and memory per field. |
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

### Known module defect (upstream `9ee21d6`; still present on `ci/valkey` `e64cd44`)

`EXHINCRBY` and `EXHINCRBYFLOAT` with an expiry replicate `PXAT (milliseconds + now)` (`src/tairhash.c:1536` and `:1780`). `milliseconds` is already absolute, so replicas get a deadline about 56 years out. Reproduced: primary `EXHPTTL` 59493 ms, replica 1794387143522 ms. `EXHSET` replicates correctly. Until the module is fixed, counters with TTLs never expire on replicas or after a failover.

## Config

### `thbench run` / `thbench reclaim` (shared flags)

| Flag | Type | Default | Description |
|---|---|---|---|
| `-addr` | string | `127.0.0.1:6379` | Server used by backends listed without `@addr`. |
| `-backends` | list | `module,lua,native` | `name[@host:port]`. Give `module` its own server: its active-expire timer SCANs every database once a second and would otherwise run against the other backends' data. |
| `-db` | int | `9` | Database the bench owns. It is **flushed** before every run. |
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
| `-memory-sample-keys` | int | `200` | Logical keys measured with exact `MEMORY USAGE` after loading. Every physical key is summed, so for `lua` that is the data hash plus its index. |

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
| `MODULE_SRC` | `..` | make | TairHash tree the module is built from. Each tree gets its own CMake build directory under `.cache/module-<mode>/`; the loaded `.so` and `source-revision` (commit plus `git status`) are per mode. |
| `MODULE_MODE` | `scan` | make | `scan`, `sort` or `slab` active-expire algorithm. |
| `MODULE_SO` | `.cache/module-scan/tairhash_module.so` | `scripts/bench.sh` | Module to load. |
| `MODULE_ARGS` | `""` | `scripts/bench.sh` | Module load args, for example `active_expire_period 100 active_expire_keys_per_loop 10000`. |
| `LABEL` | `tairhash@<rev>[+dirty][ MODULE_ARGS]` | `scripts/bench.sh` | Label passed to both `run` and `reclaim`. The default takes the first 7 characters of the commit in `source-revision` next to `MODULE_SO`, adds `+dirty` when that file lists uncommitted changes, and appends `MODULE_ARGS`. |
| `CLIENT_QOS` | `utility` | `scripts/bench.sh` (macOS) | QoS clamp the client runs under (`taskpolicy -c`). Empty disables it. `background` is too slow: the client can no longer saturate the server. See [Troubleshooting](#troubleshooting). |
| `CLIENT_CPUS` / `SERVER_CPUS` | unset | `scripts/bench.sh` (Linux) | `taskset -c` CPU lists for the client and for both servers. Give the server cores of its own. |
| `PLAIN_PORT` / `MODULE_PORT` | `26379` / `26380` | `scripts/bench.sh` | Ports of the plain and module servers. |
| `RECLAIM_ARGS` | `""` | `scripts/bench.sh` | Extra `thbench reclaim` flags. |
| `SKIP_LOAD` | unset | `scripts/bench.sh` | Set to skip the load sweep, for a reclaim-only pass into an existing `OUT` (see [Build / Run / Test](#build--run--test)). |
| `SKIP_RECLAIM` | unset | `scripts/bench.sh` | Set to skip the reclaim scenario. |
| `SKIP_PROFILE` | unset | `scripts/bench.sh` | Set to skip the profiled pass. |
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
| `gate-lock` | optional | `scripts/bench.sh` re-executes itself under `gate-lock run-exclusive` when it is on `PATH`. |
| `taskpolicy` (macOS) / `taskset` (Linux, util-linux) | — | Client and server CPU placement in `scripts/bench.sh`. |

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
make module                    # TairHash module (MODULE_MODE=scan|sort|slab, MODULE_SRC=..)
make test                      # unit tests, no server
make test-integration          # throwaway server with the module, every test with -race
make lint                      # gofmt, go vet, shellcheck

go run ./cmd/presence-demo -addr 127.0.0.1:6379 -backend module

make bench OUT=bench/results/run1 BENCH_FLAGS='-repeat 3 -duration 10s'
make report OUT=bench/results/run1
```

`scripts/bench.sh` starts two otherwise identical servers from `scripts/valkey-server.sh`: `plain` for `lua` and `native`, `module` with TairHash loaded. Both use `io-threads 1`, `hz 10`, have persistence off, and have diagnostics on: `latency-tracking-info-percentiles "50 90 99 99.9 99.99"`, `latency-monitor-threshold 1` (ms) and `slowlog-log-slower-than 1000` (µs). The script runs the measured repeats, one profiled pass and the reclaim scenario, and writes the following to `OUT`:

- `results.jsonl`
- `report.md`
- `runs/<label dir>/<run>/`
- `run.log` and `reclaim.log`
- the server logs
- `host-load.txt`: the host load before and after each pass, the label, and the CPU placement (appended across passes)

The client runs under the placement described by `CLIENT_QOS` (macOS) or `CLIENT_CPUS`/`SERVER_CPUS` (Linux). Each result records it. If a phase fails, the script skips the remaining phases, still renders `report.md` from the results recorded so far, and exits with the failing phase's status.

To measure tuned module expiry next to the defaults, run a second reclaim-only pass into the same `OUT`. The pass gets its own label from `MODULE_ARGS`, so the report keeps the two apart:

```sh
SKIP_LOAD=1 MODULE_ARGS='active_expire_period 100 active_expire_keys_per_loop 10000' \
  RECLAIM_ARGS='-backends module@127.0.0.1:26380' MODULE_SO=$PWD/.cache/module-scan/tairhash_module.so \
  scripts/bench.sh bench/results/run1
```

Re-analyse any server capture with `thbench profile RAW`, and open profiles with `go tool pprof -http=: OUT/runs/<label dir>/<run>/server.main.pprof`.

## Operations

### Reading the results

Each run brackets its measured window with `CONFIG RESETSTAT`, `SLOWLOG RESET` and `LATENCY RESET`, followed by a full `INFO everything` + `INFO debug` snapshot. It takes a second snapshot at the end. Everything below comes from the server unless marked client.

| Metric (`server_metrics.*` in `results.jsonl`) | Meaning |
|---|---|
| `ops_per_sec`, `latency.p50/p99/p999_us` (client) | Client-observed throughput and latency, including the loopback round trip and client CPU. |
| Report `vs module (paired)` | For each repeat, the backend's value divided by `module`'s in the same repeat, then the median of those ratios, with their spread. A cell's backends run back to back within a repeat, so pairing cancels host-load drift between repeats. A ratio of medians would keep that drift. |
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
| Reclaim: `server_metrics.busy_us_per_op` | Main-thread cost per reclaimed field, including module timers, core active expiry and client-driven sweeping for `lua`, net of the harness's polling. The window opens 500 ms before the deadline, so reclamation that starts the moment the deadline passes is inside it. The `lua` sweeper starts at the deadline itself. |

### Artifacts

`-artifacts-dir` gets one directory per run at `<label dir>/<run>`. The run is `<backend>-<workload>-c<conc>-r<repeat>`, `...-profile` or `reclaim-<backend>`, and is also stored as `run` in each result. The label directory is the label with every run of characters outside `[A-Za-z0-9._@+-]` collapsed to `-`. When that changes the label, an 8-hex-digit hash of the original is appended so distinct labels never share a directory. Without a label, runs sit directly under `-artifacts-dir`.

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
│   └── bench/             workloads, runner, server snapshots + metrics, timeline, artifacts, reclaim, report
├── cmd/
│   ├── thbench/           benchmark CLI (run / reclaim / report / profile)
│   └── presence-demo/     runnable use-case demo
├── scripts/               valkey-server.sh (canonical server flags), bench.sh (orchestration)
└── Makefile
```
