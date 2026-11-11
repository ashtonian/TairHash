package bench

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"

	"github.com/ashtonian/TairHash/go/internal/serverprof"
)

// preloadWorkers bounds concurrent writes while loading a keyspace.
const preloadWorkers = 64

// forEachField calls fn for every key/field pair using preloadWorkers goroutines.
func forEachField(ctx context.Context, ks *Keyspace, fn func(ctx context.Context, key, field string) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	jobs := make(chan int)
	var wg sync.WaitGroup
	for range preloadWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range jobs {
				for _, f := range ks.fields {
					if err := fn(ctx, ks.keys[k], f); err != nil {
						cancel(fmt.Errorf("load %s/%s: %w", ks.keys[k], f, err))
						return
					}
				}
			}
		}()
	}
	for k := range ks.keys {
		select {
		case jobs <- k:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return ctx.Err()
}

// RunConfig is one run.
type RunConfig struct {
	Concurrency int
	Warmup      time.Duration
	Duration    time.Duration
	Seed        uint64
	Repeat      int
	// Label is free text recorded with the result, such as the module build;
	// the report keeps each label's results apart.
	Label string
	// DB is the database the bench owns, for per-database server statistics.
	DB int
	// Client records the client options the run used.
	Client ClientFacts
	// ArtifactsDir, when set, receives the run's raw server captures.
	ArtifactsDir string
	// Placement is recorded in HostFacts.
	Placement string
	// SampleEvery is the server timeline period; zero disables the timeline.
	SampleEvery time.Duration
	// Profiled runs capture a client CPU profile and, with ServerProfiler, a
	// server profile. Profilers perturb timing, so profiled runs are reported
	// for their profiles only, never for their numbers.
	Profiled       bool
	ServerProfiler *Profiler
}

// ClientFacts are the client settings that shape a run.
type ClientFacts struct {
	Library           string `json:"library"`
	PipelineMultiplex int    `json:"pipeline_multiplex"`
	Connections       int    `json:"connections"`
}

type loadResult struct {
	ops       int64
	errors    int64
	conflicts int64
	elapsed   time.Duration
	hist      *hdrhistogram.Histogram
	firstErr  error
}

// latency histogram bounds: 1µs .. 60s at 3 significant digits.
func newHistogram() *hdrhistogram.Histogram {
	return hdrhistogram.New(int64(time.Microsecond), int64(time.Minute), 3)
}

// drive runs op on concurrency workers for d. Workers stop issuing new operations
// once d has elapsed and the in-flight ones complete, so no operation is
// abandoned mid-flight and the server counters bracket only finished work.
func drive(ctx context.Context, op Op, concurrency int, d time.Duration, seed uint64) loadResult {
	var stop atomic.Bool
	type workerOut struct {
		ops, errs, conflicts int64
		hist                 *hdrhistogram.Histogram
		firstErr             error
	}
	outs := make([]workerOut, concurrency)

	var wg sync.WaitGroup
	start := time.Now()
	for i := range concurrency {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := &Worker{rng: rand.New(rand.NewPCG(seed, uint64(i)))}
			out := workerOut{hist: newHistogram()}
			for !stop.Load() && ctx.Err() == nil {
				t0 := time.Now()
				err := op(ctx, w)
				lat := time.Since(t0)
				if err != nil {
					out.errs++
					if out.firstErr == nil {
						out.firstErr = err
					}
					continue
				}
				out.ops++
				_ = out.hist.RecordValue(int64(lat))
			}
			out.conflicts = w.conflicts
			outs[i] = out
		}(i)
	}
	timer := time.NewTimer(d)
	select {
	case <-timer.C:
	case <-ctx.Done():
		timer.Stop()
	}
	stop.Store(true)
	wg.Wait()

	res := loadResult{elapsed: time.Since(start), hist: newHistogram()}
	for _, o := range outs {
		res.ops += o.ops
		res.errors += o.errs
		res.conflicts += o.conflicts
		res.hist.Merge(o.hist)
		if res.firstErr == nil {
			res.firstErr = o.firstErr
		}
	}
	return res
}

// RunID names one run within a label: "<backend>-<workload>-c<conc>-r<repeat>",
// or "-profile" in place of the repeat for the profiled pass. It is path-safe.
func RunID(backend, workload string, concurrency, repeat int, profiled bool) string {
	if profiled {
		return fmt.Sprintf("%s-%s-c%d-profile", backend, workload, concurrency)
	}
	return fmt.Sprintf("%s-%s-c%d-r%d", backend, workload, concurrency, repeat)
}

// Run prepares wl on a flushed database, warms up, then measures one window
// with full server-side capture.
func Run(ctx context.Context, srv *Server, b Backend, wl Workload, ks *Keyspace, cfg RunConfig) (Result, error) {
	if wl.NeedsVersions && b.Versioned == nil {
		return Result{}, fmt.Errorf("workload %s needs versions, which backend %s lacks", wl.Name, b.Name)
	}
	art, err := newArtifacts(cfg.ArtifactsDir, cfg.Label, RunID(b.Name, wl.Name, cfg.Concurrency, cfg.Repeat, cfg.Profiled))
	if err != nil {
		return Result{}, err
	}
	if err := srv.Flush(ctx); err != nil {
		return Result{}, err
	}
	op, err := wl.Prepare(ctx, b, ks)
	if err != nil {
		return Result{}, fmt.Errorf("prepare %s on %s: %w", wl.Name, b.Name, err)
	}
	if cfg.Warmup > 0 {
		if w := drive(ctx, op, cfg.Concurrency, cfg.Warmup, cfg.Seed^0x5eed); w.errors > 0 {
			return Result{}, fmt.Errorf("warmup %s on %s: %d errors, first: %w", wl.Name, b.Name, w.errors, w.firstErr)
		}
	}

	facts, err := srv.Facts(ctx)
	if err != nil {
		return Result{}, err
	}
	if art.enabled() {
		cfgDump, err := srv.ConfigDump(ctx)
		if err != nil {
			return Result{}, err
		}
		if err := art.write("config.txt", cfgDump); err != nil {
			return Result{}, err
		}
	}

	if err := srv.ResetDiagnostics(ctx); err != nil {
		return Result{}, err
	}
	// The process watch brackets the server snapshots, so it covers the whole
	// server window without its own sampling time landing inside it.
	watch, err := startCPUWatch(ctx, facts.ProcessID)
	if err != nil {
		return Result{}, err
	}
	before, err := srv.Snapshot(ctx)
	if err != nil {
		return Result{}, err
	}
	load1, _ := loadAverage()

	prof, err := startProfiles(ctx, cfg.Profiled, cfg.ServerProfiler, cfg.Duration, art, facts.ProcessID)
	if err != nil {
		return Result{}, err
	}
	timeline := startSampler(ctx, srv, cfg.SampleEvery)
	lr := drive(ctx, op, cfg.Concurrency, cfg.Duration, cfg.Seed)
	samples, sampleErr := timeline.stop()
	// Snapshot before waiting on the profiler, which may outlive the window.
	after, err := srv.Snapshot(ctx)
	if err != nil {
		return Result{}, err
	}
	interference, err := watch.stop(ctx)
	if err != nil {
		return Result{}, err
	}
	load2, _ := loadAverage()
	if err := prof.stop(); err != nil {
		return Result{}, err
	}
	if lr.errors > 0 {
		return Result{}, fmt.Errorf("run %s on %s: %d errors in %d ops, first: %w", wl.Name, b.Name, lr.errors, lr.ops+lr.errors, lr.firstErr)
	}
	if sampleErr != nil {
		return Result{}, fmt.Errorf("server timeline: %w", sampleErr)
	}

	metrics, err := Measure(before, after, lr.ops, cfg.DB)
	if err != nil {
		return Result{}, err
	}
	if err := captureDiagnostics(ctx, srv, art, &metrics); err != nil {
		return Result{}, err
	}
	metrics.Timeline = timelineStats(samples)

	res := newResult(b, wl, ks, cfg, lr, metrics, facts, [2]float64{load1, load2})
	res.Interference = interference
	res.Artifacts = art.dir
	res.ClientProfile = prof.clientPath
	if res.ServerProfile, err = prof.convertServer(ctx, cfg.ServerProfiler, art, facts.ProcessID); err != nil {
		return Result{}, err
	}

	for name, content := range map[string]string{
		"info.before.txt":          before.Raw,
		"info.after.txt":           after.Raw,
		"module-expire.before.txt": before.ModuleExpire,
		"module-expire.after.txt":  after.ModuleExpire,
	} {
		if content == "" {
			continue
		}
		if err := art.write(name, content); err != nil {
			return Result{}, err
		}
	}
	if err := writeJSONLines(art, "timeline.jsonl", samples); err != nil {
		return Result{}, err
	}
	if err := art.writeJSON("result.json", res); err != nil {
		return Result{}, err
	}
	// Leave the server empty: while other backends run, a loaded module keeps
	// scanning its data from the active-expire timer.
	if err := srv.Flush(ctx); err != nil {
		return Result{}, err
	}
	return res, nil
}

// captureDiagnostics adds the slowlog and latency-monitor events of the window
// to m and stores their full detail plus MEMORY STATS in the artifacts.
func captureDiagnostics(ctx context.Context, srv *Server, art artifacts, m *ServerMetrics) error {
	slow, err := srv.Slowlog(ctx)
	if err != nil {
		return err
	}
	m.Slowlog = summarizeSlowlog(slow)
	events, err := srv.LatencyLatest(ctx)
	if err != nil {
		return err
	}
	m.LatencyEvents = events
	if !art.enabled() {
		return nil
	}
	history := map[string][][2]int64{}
	for _, e := range events {
		if history[e.Event], err = srv.LatencyHistory(ctx, e.Event); err != nil {
			return err
		}
	}
	memStats, err := srv.MemoryStats(ctx)
	if err != nil {
		return err
	}
	if err := art.writeJSON("slowlog.json", slow); err != nil {
		return err
	}
	if err := art.writeJSON("latency.json", map[string]any{"latest": events, "history": history}); err != nil {
		return err
	}
	return art.write("memory-stats.txt", memStats)
}

type profiles struct {
	clientPath string
	serverRaw  string
	stop       func() error
}

// convertServer turns the raw server capture into pprof and a phase summary;
// it returns nil when no server profile was taken.
func (p profiles) convertServer(ctx context.Context, prof *Profiler, art artifacts, pid int) (*ServerProfile, error) {
	if p.serverRaw == "" {
		return nil, nil
	}
	sum, outs, err := serverprof.Convert(ctx, prof.Format, p.serverRaw, pid, prof.Period, art.path("server"), 30)
	if err != nil {
		return nil, fmt.Errorf("convert server profile: %w", err)
	}
	return &ServerProfile{Raw: p.serverRaw, Outputs: outs, Summary: sum}, nil
}

// startProfiles starts the client CPU profile and, with prof, a server profile
// of d when profiled is set; otherwise it does nothing.
func startProfiles(ctx context.Context, profiled bool, prof *Profiler, d time.Duration, art artifacts, pid int) (profiles, error) {
	p := profiles{stop: func() error { return nil }}
	if !profiled {
		return p, nil
	}
	if !art.enabled() {
		return p, errors.New("profiled runs need an artifacts directory")
	}
	p.clientPath = art.path("client.pprof")
	f, err := os.Create(p.clientPath)
	if err != nil {
		return p, fmt.Errorf("create client profile: %w", err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		f.Close()
		return p, fmt.Errorf("start client profile: %w", err)
	}
	var waitServer func() error
	if prof != nil && pid > 0 {
		p.serverRaw = art.path(prof.rawName())
		if waitServer, err = prof.Start(ctx, pid, d, p.serverRaw); err != nil {
			pprof.StopCPUProfile()
			f.Close()
			return p, err
		}
	}
	p.stop = func() error {
		pprof.StopCPUProfile()
		cerr := f.Close()
		if waitServer != nil {
			if err := waitServer(); err != nil {
				return fmt.Errorf("server profiler: %w", err)
			}
		}
		return cerr
	}
	return p, nil
}

// HostFacts describes the client side of a run.
type HostFacts struct {
	GoVersion  string `json:"go_version"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
	NumCPU     int    `json:"num_cpu"`
	GOMAXPROCS int    `json:"gomaxprocs"`
	// Placement is how the client and servers were bound to CPUs, as stated by
	// whoever launched them (for example "client: taskpolicy -c utility").
	Placement string `json:"placement,omitempty"`
}

func hostFacts(placement string) HostFacts {
	return HostFacts{
		GoVersion:  runtime.Version(),
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		NumCPU:     runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		Placement:  placement,
	}
}
