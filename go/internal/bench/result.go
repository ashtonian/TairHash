package bench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ashtonian/TairHash/go/internal/serverprof"
)

// Latency summarises the client-observed latency distribution in microseconds.
type Latency struct {
	P50  float64 `json:"p50_us"`
	P90  float64 `json:"p90_us"`
	P99  float64 `json:"p99_us"`
	P999 float64 `json:"p999_us"`
	Max  float64 `json:"max_us"`
	Mean float64 `json:"mean_us"`
}

// ServerProfile is a profiled run's server capture and its analysis.
type ServerProfile struct {
	Raw     string             `json:"raw"`
	Outputs serverprof.Outputs `json:"outputs"`
	Summary serverprof.Summary `json:"summary"`
}

// Result is one run, written as one JSON line.
type Result struct {
	Kind        string    `json:"kind"`
	Time        time.Time `json:"time"`
	Label       string    `json:"label"`
	Run         string    `json:"run"`
	Backend     string    `json:"backend"`
	BackendType string    `json:"backend_type"`
	Workload    string    `json:"workload"`
	Concurrency int       `json:"concurrency"`
	Repeat      int       `json:"repeat"`
	// Profiled runs exist for their profiles; their timings are perturbed by
	// the profilers and are excluded from reported figures.
	Profiled  bool   `json:"profiled"`
	Keys      int    `json:"keys"`
	Fields    int    `json:"fields_per_key"`
	ValueSize int    `json:"value_size"`
	TTL       string `json:"ttl"`
	Duration  string `json:"duration"`
	Warmup    string `json:"warmup"`
	Seed      uint64 `json:"seed"`

	Ops        int64   `json:"ops"`
	Conflicts  int64   `json:"conflicts,omitempty"`
	ElapsedSec float64 `json:"elapsed_sec"`
	OpsPerSec  float64 `json:"ops_per_sec"`
	Latency    Latency `json:"latency"`

	Server        ServerMetrics  `json:"server_metrics"`
	Interference  Interference   `json:"interference"`
	ServerProfile *ServerProfile `json:"server_profile,omitempty"`
	ClientProfile string         `json:"client_profile,omitempty"`
	Artifacts     string         `json:"artifacts,omitempty"`

	ServerFacts Facts       `json:"server"`
	Host        HostFacts   `json:"host"`
	Client      ClientFacts `json:"client"`
	LoadAvg     [2]float64  `json:"load_avg_1m_start_end"`
}

func newResult(b Backend, wl Workload, ks *Keyspace, cfg RunConfig, lr loadResult, m ServerMetrics, facts Facts, load [2]float64) Result {
	us := func(q float64) float64 { return float64(lr.hist.ValueAtQuantile(q)) / 1e3 }
	r := Result{
		Kind:        "load",
		Time:        time.Now().UTC(),
		Label:       cfg.Label,
		Run:         RunID(b.Name, wl.Name, cfg.Concurrency, cfg.Repeat, cfg.Profiled),
		Backend:     b.Name,
		BackendType: b.Type,
		Workload:    wl.Name,
		Concurrency: cfg.Concurrency,
		Repeat:      cfg.Repeat,
		Profiled:    cfg.Profiled,
		Keys:        ks.Keys,
		Fields:      ks.Fields,
		ValueSize:   ks.ValueSize,
		TTL:         ks.TTL.String(),
		Duration:    cfg.Duration.String(),
		Warmup:      cfg.Warmup.String(),
		Seed:        cfg.Seed,
		Ops:         lr.ops,
		Conflicts:   lr.conflicts,
		ElapsedSec:  lr.elapsed.Seconds(),
		Latency: Latency{
			P50:  us(50),
			P90:  us(90),
			P99:  us(99),
			P999: us(99.9),
			Max:  float64(lr.hist.Max()) / 1e3,
			Mean: lr.hist.Mean() / 1e3,
		},
		Server:      m,
		ServerFacts: facts,
		Host:        hostFacts(cfg.Placement),
		Client:      cfg.Client,
		LoadAvg:     load,
	}
	if lr.ops > 0 {
		r.OpsPerSec = float64(lr.ops) / lr.elapsed.Seconds()
	}
	return r
}

// Writer appends JSON lines to a file.
type Writer struct {
	f *os.File
}

// OpenWriter opens path for appending, creating it if needed.
func OpenWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open results %s: %w", path, err)
	}
	return &Writer{f: f}, nil
}

// Write appends v as one line and syncs it, so an interrupted sweep keeps
// every completed run.
func (w *Writer) Write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return w.f.Sync()
}

// Close closes the file.
func (w *Writer) Close() error { return w.f.Close() }

// ReadResults decodes a JSON-lines results file into load and reclaim results.
func ReadResults(r io.Reader) ([]Result, []ReclaimResult, error) {
	var loads []Result
	var reclaims []ReclaimResult
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for line := 1; sc.Scan(); line++ {
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(sc.Bytes(), &kind); err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", line, err)
		}
		switch kind.Kind {
		case "load":
			var res Result
			if err := json.Unmarshal(sc.Bytes(), &res); err != nil {
				return nil, nil, fmt.Errorf("line %d: %w", line, err)
			}
			loads = append(loads, res)
		case "reclaim":
			var res ReclaimResult
			if err := json.Unmarshal(sc.Bytes(), &res); err != nil {
				return nil, nil, fmt.Errorf("line %d: %w", line, err)
			}
			reclaims = append(reclaims, res)
		default:
			return nil, nil, fmt.Errorf("line %d: unknown result kind %q", line, kind.Kind)
		}
	}
	return loads, reclaims, sc.Err()
}
