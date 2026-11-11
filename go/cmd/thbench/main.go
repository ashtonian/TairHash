// Command thbench benchmarks fieldhash backends — the TairHash module, its Lua
// (Valkey Functions) emulation and Valkey's native hash-field expiration —
// against one Valkey server.
//
//	thbench run    [flags]   measure workloads, append JSON lines to -out
//	thbench reclaim [flags]  measure expiry reclamation and memory per field
//	thbench report [-reference NAME] FILE...   render results as Markdown
//	thbench profile [flags] RAW  convert a raw server capture to pprof and a phase summary
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/ashtonian/TairHash/go/fieldhash/lua"
	"github.com/ashtonian/TairHash/go/fieldhash/module"
	"github.com/ashtonian/TairHash/go/fieldhash/native"
	"github.com/ashtonian/TairHash/go/internal/bench"
	"github.com/ashtonian/TairHash/go/internal/serverprof"
	"github.com/ashtonian/TairHash/go/tairhash"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(ctx, log, os.Args[2:])
	case "reclaim":
		err = reclaimCmd(ctx, log, os.Args[2:])
	case "report":
		err = reportCmd(os.Args[2:])
	case "profile":
		err = profileCmd(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error("thbench failed", slog.String("command", os.Args[1]), slog.Any("error", err))
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: thbench run|reclaim [flags] | thbench report [-reference NAME] FILE... | thbench profile [flags] RAW")
}

// common holds the flags shared by run and reclaim.
type common struct {
	addr         string
	db           int
	flush        bool
	backends     string
	keys         int
	fields       int
	valueSize    int
	out          string
	label        string
	multiplex    int
	writeTimeout time.Duration
	artifacts    string
	placement    string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.addr, "addr", "127.0.0.1:6379", "Valkey address for backends given without @addr")
	fs.IntVar(&c.db, "db", 9, "database the bench owns; it is flushed before every run")
	fs.BoolVar(&c.flush, "flush", false, "allow flushing -db when it already holds keys")
	fs.StringVar(&c.backends, "backends", "module,lua,native",
		"comma-separated [name=]type[@host:port]; types: module, lua, native; name defaults to the type and must be "+
			"unique, so builds of one type are told apart by name. Give each module its own server so its "+
			"active-expire timer does not run against the other backends' data")
	fs.IntVar(&c.keys, "keys", 10_000, "number of hashes")
	fs.IntVar(&c.fields, "fields", 10, "fields per hash")
	fs.IntVar(&c.valueSize, "value-size", 64, "bytes per field value")
	fs.StringVar(&c.out, "out", "results.jsonl", "JSON-lines file results are appended to")
	fs.StringVar(&c.label, "label", "", "label recorded with every result (for example the server build)")
	fs.IntVar(&c.multiplex, "pipeline-multiplex", 2, "valkey-go PipelineMultiplex: 2^n pipelined connections")
	fs.DurationVar(&c.writeTimeout, "conn-write-timeout", 10*time.Second, "valkey-go ConnWriteTimeout")
	fs.StringVar(&c.artifacts, "artifacts-dir", "", "directory for per-run raw server captures (INFO, config, slowlog, latency, timeline, profiles)")
	fs.StringVar(&c.placement, "placement", "", "how the client and servers are bound to CPUs, recorded with every result (scripts/bench.sh sets it)")
}

// target is one backend bound to the server it runs against.
type target struct {
	bench.Backend
	addr string
	vk   valkey.Client
	srv  *bench.Server
}

type env struct {
	targets []target
	results *bench.Writer
	client  bench.ClientFacts
}

func (e *env) close() {
	if e.results != nil {
		_ = e.results.Close()
	}
	for _, t := range e.targets {
		t.vk.Close()
	}
}

func setup(ctx context.Context, log *slog.Logger, c common) (*env, error) {
	e := &env{client: bench.ClientFacts{Library: "valkey-go v1.0.79", PipelineMultiplex: c.multiplex, Connections: 1 << c.multiplex}}
	ok := false
	defer func() {
		if !ok {
			e.close()
		}
	}()

	specs, err := bench.ParseBackendSpecs(c.backends)
	if err != nil {
		return nil, err
	}
	for _, spec := range specs {
		name, addr := spec.Name, spec.Addr
		if addr == "" {
			addr = c.addr
		}
		vk, err := valkey.NewClient(valkey.ClientOption{
			InitAddress:       []string{addr},
			SelectDB:          c.db,
			DisableCache:      true,
			PipelineMultiplex: c.multiplex,
			ConnWriteTimeout:  c.writeTimeout,
		})
		if err != nil {
			return nil, fmt.Errorf("backend %s: connect %s: %w", name, addr, err)
		}
		t := target{addr: addr, vk: vk, srv: bench.NewServer(vk, c.db)}
		e.targets = append(e.targets, t)

		keys, err := t.srv.Keys(ctx)
		if err != nil {
			return nil, fmt.Errorf("backend %s: %w", name, err)
		}
		if keys > 0 && !c.flush {
			return nil, fmt.Errorf("backend %s: %s db %d holds %d keys; pass -flush to let the bench erase it", name, addr, c.db, keys)
		}
		if e.targets[len(e.targets)-1].Backend, err = newBackend(ctx, vk, spec); err != nil {
			return nil, err
		}
		facts, err := t.srv.Facts(ctx)
		if err != nil {
			return nil, fmt.Errorf("backend %s: %w", name, err)
		}
		log.Info("backend ready",
			slog.String("backend", name), slog.String("type", spec.Type), slog.String("addr", addr), slog.Int("db", c.db),
			slog.String("valkey_version", facts.ValkeyVersion), slog.Any("modules", facts.Modules),
			slog.String("tairhash_revision", facts.TairHashRevision),
			slog.Any("config", facts.Config), slog.Any("tairhash_active_expire", facts.ModuleExpire),
			slog.Any("client", e.client))
	}

	if e.results, err = bench.OpenWriter(c.out); err != nil {
		return nil, err
	}
	ok = true
	return e, nil
}

func newBackend(ctx context.Context, vk valkey.Client, spec bench.BackendSpec) (bench.Backend, error) {
	b := bench.Backend{Name: spec.Name, Type: spec.Type}
	switch spec.Type {
	case "module":
		c := tairhash.New(vk)
		if err := c.Ping(ctx); err != nil {
			return bench.Backend{}, fmt.Errorf("backend %s: %w", spec.Name, err)
		}
		s := module.New(c)
		b.Store, b.Versioned = s, s
	case "lua":
		s, err := lua.New(ctx, vk)
		if err != nil {
			return bench.Backend{}, fmt.Errorf("backend %s: %w", spec.Name, err)
		}
		b.Store, b.Versioned, b.Sweeper = s, s, s
		b.PhysicalKeys = func(k string) []string { return []string{lua.DataKey(k), lua.IndexKey(k)} }
	case "native":
		docs, err := vk.Do(ctx, vk.B().CommandInfo().CommandName("HSETEX").Build()).ToArray()
		if err != nil || len(docs) == 0 || docs[0].IsNil() {
			return bench.Backend{}, fmt.Errorf("backend %s: server lacks HSETEX (needs Valkey >= 9.0): %v", spec.Name, err)
		}
		b.Store = native.New(vk)
	default:
		return bench.Backend{}, fmt.Errorf("backend %s: unknown type %q", spec.Name, spec.Type)
	}
	return b, nil
}

func runCmd(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c common
	c.register(fs)
	workloads := fs.String("workloads", "set-ttl,get,getall,incr-ttl,mixed,presence,cas", "comma-separated workloads")
	concurrency := fs.String("concurrency", "1,16,128", "comma-separated client concurrency levels")
	duration := fs.Duration("duration", 10*time.Second, "measured window per run")
	warmup := fs.Duration("warmup", 3*time.Second, "unmeasured warmup per run")
	repeat := fs.Int("repeat", 3, "runs per cell; the report takes medians")
	ttl := fs.Duration("ttl", time.Hour, "lease on written fields; must outlast the run")
	seed := fs.Uint64("seed", 1, "base seed for key selection")
	profile := fs.Bool("profile", false, "after the measured repeats, run every cell once more with client and server profilers (needs -artifacts-dir); profiled runs are excluded from reported figures")
	profileConc := fs.String("profile-concurrency", "", "comma-separated concurrency levels the profiled pass covers (default: every -concurrency level)")
	profilerKind := fs.String("server-profiler", "auto", "server profiler for -profile: auto, sample (macOS), perf (Linux) or none")
	profilerCmd := fs.String("server-profiler-cmd", "", "override the profiler command; {pid} {secs} {out} are substituted")
	timelineEvery := fs.Duration("timeline-every", time.Second, "server timeline sampling period during measured windows; 0 disables")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *profile && c.artifacts == "" {
		return errors.New("-profile needs -artifacts-dir")
	}

	var wls []bench.Workload
	for _, name := range splitList(*workloads) {
		wl, ok := bench.LookupWorkload(name)
		if !ok {
			return fmt.Errorf("unknown workload %q", name)
		}
		wls = append(wls, wl)
	}
	concs, err := parseInts(*concurrency)
	if err != nil {
		return fmt.Errorf("-concurrency: %w", err)
	}
	profileConcs := concs
	if *profileConc != "" {
		if profileConcs, err = parseInts(*profileConc); err != nil {
			return fmt.Errorf("-profile-concurrency: %w", err)
		}
		for _, pc := range profileConcs {
			if !slices.Contains(concs, pc) {
				return fmt.Errorf("-profile-concurrency %d is not one of -concurrency %s", pc, *concurrency)
			}
		}
	}
	ks, err := bench.NewKeyspace("bench:", c.keys, c.fields, c.valueSize, *ttl)
	if err != nil {
		return err
	}
	var prof *bench.Profiler
	if *profile {
		if prof, err = bench.NewProfiler(*profilerKind, *profilerCmd); err != nil {
			return err
		}
		if prof == nil {
			log.Warn("no server profiler available; profiled pass captures client profiles only", slog.String("server_profiler", *profilerKind))
		}
	}

	e, err := setup(ctx, log, c)
	if err != nil {
		return err
	}
	defer e.close()

	// Backends are interleaved inside each cell so slow drift on a shared host
	// (thermal state, other tenants) lands on every backend alike. The profiled
	// pass runs last and only for its profiles: profilers perturb timing.
	passes := *repeat
	if *profile {
		passes++
	}
	for rep := range passes {
		profiled := rep == *repeat
		for _, wl := range wls {
			for _, conc := range concs {
				if profiled && !slices.Contains(profileConcs, conc) {
					continue
				}
				for _, t := range e.targets {
					if wl.NeedsVersions && t.Versioned == nil {
						continue
					}
					cfg := bench.RunConfig{
						Concurrency: conc, Warmup: *warmup, Duration: *duration,
						Seed: *seed + uint64(rep), Repeat: rep, Label: c.label, DB: c.db,
						Client: e.client, ArtifactsDir: c.artifacts, Placement: c.placement, SampleEvery: *timelineEvery,
						Profiled: profiled, ServerProfiler: prof,
					}
					res, err := bench.Run(ctx, t.srv, t.Backend, wl, ks, cfg)
					if err != nil {
						return err
					}
					if err := e.results.Write(res); err != nil {
						return err
					}
					attrs := []any{
						slog.String("label", res.Label), slog.String("backend", res.Backend), slog.String("run", res.Run), slog.Bool("profiled", profiled),
						slog.Float64("ops_per_sec", res.OpsPerSec),
						slog.Float64("p50_us", res.Latency.P50), slog.Float64("p99_us", res.Latency.P99),
						slog.Float64("server_busy_us_per_op", res.Server.BusyUsPerOp),
						slog.Float64("server_utilization", res.Server.Utilization),
						slog.Float64("eventloop_cmd_us_per_op", res.Server.EventLoop.CmdUsPerOp),
						slog.Float64("ops_per_cycle", res.Server.EventLoop.OpsPerCycle),
						slog.Float64("commands_per_op", res.Server.CommandsPerOp),
						slog.Int("slowlog_entries", res.Server.Slowlog.Entries),
						slog.Float64("foreign_cores", res.Interference.ForeignCores),
						slog.Any("load_avg", res.LoadAvg),
					}
					if res.ServerProfile != nil {
						attrs = append(attrs, slog.Float64("profile_busy_percent", res.ServerProfile.Summary.BusyPercent),
							slog.String("server_pprof", res.ServerProfile.Outputs.MainPprof))
					}
					log.Info("run complete", attrs...)
				}
			}
		}
	}
	return nil
}

func reclaimCmd(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("reclaim", flag.ExitOnError)
	var c common
	c.register(fs)
	lease := fs.Duration("lease", 30*time.Second, "time from load start to the shared expiry deadline")
	timeout := fs.Duration("timeout", 5*time.Minute, "give up waiting for reclamation after this")
	poll := fs.Duration("poll", 100*time.Millisecond, "DBSIZE sampling interval")
	idle := fs.Duration("idle-window", 3*time.Second, "idle-server measurement before loading; its busy rate is subtracted from the reclaim cost (0 disables)")
	sweepEvery := fs.Duration("sweep-every", 100*time.Millisecond, "sweeper period for the lua backend")
	sweepBudget := fs.Int("sweep-budget", 10_000, "fields the lua sweeper may reclaim per period")
	memSample := fs.Int("memory-sample-keys", 200, "logical keys measured with MEMORY USAGE after loading")
	profile := fs.Bool("profile", false, "after the measured reclaims, reclaim on every backend once more under client and server profilers, stopping when the profile ends (needs -artifacts-dir); profiled runs are excluded from reported figures")
	profileWindow := fs.Duration("profile-window", 10*time.Second, "server profile length for -profile, from the opening of the reclaim window")
	profilerKind := fs.String("server-profiler", "auto", "server profiler for -profile: auto, sample (macOS), perf (Linux) or none")
	profilerCmd := fs.String("server-profiler-cmd", "", "override the profiler command; {pid} {secs} {out} are substituted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *profile && c.artifacts == "" {
		return errors.New("-profile needs -artifacts-dir")
	}
	var prof *bench.Profiler
	if *profile {
		var err error
		if prof, err = bench.NewProfiler(*profilerKind, *profilerCmd); err != nil {
			return err
		}
		if prof == nil {
			log.Warn("no server profiler available; profiled reclaim captures client profiles only", slog.String("server_profiler", *profilerKind))
		}
	}
	ks, err := bench.NewKeyspace("reclaim:", c.keys, c.fields, c.valueSize, *lease)
	if err != nil {
		return err
	}
	e, err := setup(ctx, log, c)
	if err != nil {
		return err
	}
	defer e.close()

	passes := []bool{false}
	if *profile {
		passes = append(passes, true)
	}
	for _, profiled := range passes {
		for _, t := range e.targets {
			if err := reclaimOne(ctx, log, e, t, ks, bench.ReclaimConfig{
				Lease: *lease, Timeout: *timeout, Poll: *poll, IdleWindow: *idle,
				SweepEvery: *sweepEvery, SweepBudget: *sweepBudget, MemorySampleKeys: *memSample,
				Label: c.label, DB: c.db, ArtifactsDir: c.artifacts, Placement: c.placement,
				Profiled: profiled, ServerProfiler: prof, ProfileWindow: *profileWindow,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func reclaimOne(ctx context.Context, log *slog.Logger, e *env, t target, ks *bench.Keyspace, cfg bench.ReclaimConfig) error {
	res, err := bench.Reclaim(ctx, t.srv, t.Backend, ks, cfg)
	if err != nil {
		return fmt.Errorf("reclaim %s: %w", t.Name, err)
	}
	if err := e.results.Write(res); err != nil {
		return err
	}
	attrs := []any{
		slog.String("backend", t.Name), slog.Bool("profiled", res.Profiled), slog.Bool("reclaimed", res.Reclaimed),
		slog.Float64("reclaim_sec", res.ReclaimSec), slog.Float64("fields_per_sec", res.FieldsPerSec),
		slog.Float64("bytes_per_field", res.BytesPerField),
		slog.Float64("memory_usage_per_field", res.MemoryUsagePerField),
		slog.Float64("server_busy_us_per_field", res.BusyUsPerField),
		slog.Float64("idle_busy_per_sec", res.IdleBusyPerSec),
		slog.Int64("server_expired_fields", res.Server.ExpiredFields),
		slog.Int64("module_active_expired", res.Server.ModuleActiveExpired),
		slog.Int64("remaining_keys", res.RemainingKeys),
		slog.Float64("foreign_cores", res.Interference.ForeignCores),
	}
	if res.ServerProfile != nil {
		attrs = append(attrs, slog.Float64("profile_busy_percent", res.ServerProfile.Summary.BusyPercent),
			slog.String("server_pprof", res.ServerProfile.Outputs.MainPprof))
	}
	log.Info("reclaim complete", attrs...)
	return nil
}

func reportCmd(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	reference := fs.String("reference", "", "backend every other is compared against (default: module when present, else the first backend of each label)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	files := fs.Args()
	if len(files) == 0 {
		return errors.New("report needs at least one results file")
	}
	var loads []bench.Result
	var reclaims []bench.ReclaimResult
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		l, r, err := bench.ReadResults(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		loads, reclaims = append(loads, l...), append(reclaims, r...)
	}
	return bench.Report(os.Stdout, loads, reclaims, bench.ReportOptions{Reference: *reference})
}

func profileCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("profile", flag.ExitOnError)
	format := fs.String("format", "sample", "raw capture format: sample (macOS) or perf (Linux perf.data)")
	pid := fs.Int("pid", 0, "server pid; perf needs it to identify the main thread")
	period := fs.Duration("period", time.Millisecond, "sampling period of the capture")
	prefix := fs.String("out", "", "output path prefix (default: RAW without extension)")
	top := fs.Int("top", 25, "functions listed in the summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("profile needs exactly one raw capture")
	}
	raw := fs.Arg(0)
	if *prefix == "" {
		*prefix = strings.TrimSuffix(raw, filepath.Ext(raw))
	}
	sum, out, err := serverprof.Convert(ctx, serverprof.Format(*format), raw, *pid, *period, *prefix, *top)
	if err != nil {
		return err
	}
	return bench.WriteProfileSummary(os.Stdout, sum, out)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, p := range splitList(s) {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%q is not a positive integer", p)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("empty list")
	}
	return out, nil
}
