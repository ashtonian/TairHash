package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashtonian/TairHash/go/fieldhash"
)

func TestParseInfo(t *testing.T) {
	raw := "# CPU\r\nused_cpu_sys:0.339371\r\neventloop_duration_sum:269\r\n\r\n# Commandstats\r\n" +
		"cmdstat_fcall:calls=3,usec=51,usec_per_call=17.00,rejected_calls=1,failed_calls=2\r\n" +
		"cmdstat_config|resetstat:calls=1,usec=48,usec_per_call=48.00,rejected_calls=0,failed_calls=0\r\n" +
		"# Latencystats\r\nlatency_percentiles_usec_fcall:p50=12.031,p99=40.191,p99.9=80.383\r\n" +
		"# Errorstats\r\nerrorstat_ERR:count=4\r\n"
	snap := Snapshot{Info: parseInfoText(raw)}
	assert.Equal(t, 0.339371, snap.float("used_cpu_sys"))

	stats, err := snap.cmdStats()
	require.NoError(t, err)
	assert.Equal(t, CmdStat{Calls: 3, Usec: 51, Rejected: 1, Failed: 2}, stats["fcall"])
	assert.Contains(t, stats, "config|resetstat", "subcommand stats keep their full name")
	assert.Equal(t, map[string]float64{"p50": 12.031, "p99": 40.191, "p99.9": 80.383}, snap.latencyPercentiles()["fcall"])
	assert.Equal(t, map[string]int64{"ERR": 4}, snap.errorStats())
}

func TestMeasure(t *testing.T) {
	t0 := time.Unix(100, 0)
	before := Snapshot{At: t0, Info: map[string]string{
		"used_cpu_user": "2.0", "used_cpu_sys": "1.0",
		"total_commands_processed": "100", "eventloop_cycles": "10", "eventloop_duration_sum": "1000",
		"eventloop_duration_cmd_sum": "400", "total_reads_processed": "50", "total_writes_processed": "50",
		"total_net_input_bytes": "1000", "total_net_output_bytes": "500", "used_memory": "1000",
		"used_memory_dataset": "100",
	}}
	after := Snapshot{At: t0.Add(2 * time.Second), Info: map[string]string{
		"used_cpu_user": "3.0", "used_cpu_sys": "1.5",
		"total_commands_processed": "3100", "eventloop_cycles": "110", "eventloop_duration_sum": "1001000",
		"eventloop_duration_cmd_sum": "800400", "total_reads_processed": "150", "total_writes_processed": "130",
		"total_net_input_bytes": "61000", "total_net_output_bytes": "20500", "used_memory": "5000",
		"used_memory_dataset": "2100", "db9": "keys=10,expires=0,avg_ttl=0",
		"cmdstat_fcall":                  "calls=1000,usec=8000,usec_per_call=8.00,rejected_calls=0,failed_calls=0",
		"cmdstat_info":                   "calls=2,usec=100000,usec_per_call=50000.00,rejected_calls=0,failed_calls=0",
		"latency_percentiles_usec_fcall": "p50=7.0,p99=20.0,p99.9=30.0",
	}}
	m, err := Measure(before, after, 1000, 9)
	require.NoError(t, err)

	assert.InDelta(t, 2.0, m.WindowSec, 1e-9)
	assert.InDelta(t, 1.0, m.BusyMainSec, 1e-9)
	assert.InDelta(t, 0.5, m.Utilization, 1e-9)
	assert.InDelta(t, 0.1, m.HarnessSec, 1e-9, "harness INFO time is accounted")
	assert.InDelta(t, 900, m.BusyUsPerOp, 1e-9, "per-op cost excludes harness commands")
	assert.InDelta(t, 1500, m.CPUUsPerOp, 1e-9)
	assert.InDelta(t, 3.0, m.CommandsPerOp, 1e-9)
	assert.InDelta(t, 1000, m.EventLoop.UsPerOp, 1e-9)
	assert.InDelta(t, 800, m.EventLoop.CmdUsPerOp, 1e-9)
	assert.InDelta(t, 200, m.EventLoop.NonCmdUsPerOp, 1e-9)
	assert.InDelta(t, 10.0, m.EventLoop.OpsPerCycle, 1e-9)
	assert.InDelta(t, 0.1, m.IO.ReadsPerOp, 1e-9)
	assert.InDelta(t, 60.0, m.IO.InBytesPerOp, 1e-9)
	assert.InDelta(t, 20.0, m.IO.OutBytesPerOp, 1e-9)
	assert.Equal(t, int64(4000), m.Memory.UsedMemoryDelta)
	assert.Equal(t, int64(2000), m.Memory.DatasetDelta)
	assert.Equal(t, "keys=10,expires=0,avg_ttl=0", m.Keyspace)
	assert.Equal(t, CommandMetrics{
		Calls: 1000, Usec: 8000, UsecPerCall: 8, CallsPerOp: 1,
		Latency: map[string]float64{"p50": 7, "p99": 20, "p99.9": 30},
	}, m.PerCommand["fcall"])
}

func TestModuleExpireCounts(t *testing.T) {
	text := "# Active expire statistics\r\nenable_active_expire:1\r\n\r\n# DB detail statistics\r\n" +
		"db: 0, active_expired_fields: 5, passive_expired_fields: 1\r\n" +
		"db: 9, active_expired_fields: 1200, passive_expired_fields: 34\r\n"
	a, p, ok := moduleExpireCounts(text, 9)
	assert.True(t, ok)
	assert.Equal(t, int64(1200), a)
	assert.Equal(t, int64(34), p)
	_, _, ok = moduleExpireCounts(text, 3)
	assert.False(t, ok)
}

func TestMedianAndSpread(t *testing.T) {
	assert.Equal(t, 2.0, median([]float64{3, 1, 2}))
	assert.Equal(t, 2.5, median([]float64{4, 1, 2, 3}))
	assert.Equal(t, 0.0, median(nil))
	assert.InDelta(t, 1.0, spread([]float64{1, 2, 3}), 1e-9)
	assert.Equal(t, 0.0, spread([]float64{5}))
}

func TestReportExcludesProfiledRuns(t *testing.T) {
	loads := []Result{
		{Workload: "get", Concurrency: 16, Backend: "module", OpsPerSec: 100, Server: ServerMetrics{BusyUsPerOp: 2}},
		{Workload: "get", Concurrency: 16, Backend: "lua", OpsPerSec: 50, Server: ServerMetrics{BusyUsPerOp: 5}},
		{Workload: "get", Concurrency: 16, Backend: "lua", OpsPerSec: 1, Profiled: true, Server: ServerMetrics{BusyUsPerOp: 99}},
	}
	var out strings.Builder
	require.NoError(t, Report(&out, Results{Loads: loads}, ReportOptions{}))
	assert.Contains(t, out.String(), "| 16 | lua | 1 | 50 (±0%) |")
	assert.Contains(t, out.String(), "| 16 | lua | 5.00 | 2.50x (±0%) |")
}

func TestPairedRatioCancelsDriftBetweenRepeats(t *testing.T) {
	runs := func(vals ...float64) *aggregate {
		a := &aggregate{}
		for i, v := range vals {
			a.runs = append(a.runs, Result{Repeat: i, OpsPerSec: v})
		}
		return a
	}
	ops := func(r Result) float64 { return r.OpsPerSec }
	// Repeat 1 ran on a loaded host. Per-repeat ratios are 0.5, 0.5, 0.75; a
	// ratio of medians would report 50/80 = 0.62.
	module, lua := runs(100, 50, 80), runs(50, 25, 60)
	assert.Equal(t, "0.50x (±25%)", formatPairedRatio(lua, module, ops))
	assert.Equal(t, "—", formatPairedRatio(lua, nil, ops))
	assert.Equal(t, "—", formatPairedRatio(runs(), module, ops))
}

func TestReportKeepsLabelsApart(t *testing.T) {
	loads := []Result{
		{Label: "build-a", Workload: "get", Concurrency: 1, Backend: "module", OpsPerSec: 100},
		{Label: "build-b", Workload: "get", Concurrency: 1, Backend: "module", OpsPerSec: 300},
	}
	var out strings.Builder
	require.NoError(t, Report(&out, Results{Loads: loads}, ReportOptions{}))
	assert.Contains(t, out.String(), "\n# build-a\n")
	assert.Contains(t, out.String(), "\n# build-b\n")
	assert.Contains(t, out.String(), "| 1 | module | 1 | 100 (±0%) |")
	assert.Contains(t, out.String(), "| 1 | module | 1 | 300 (±0%) |")
}

func TestLabelDir(t *testing.T) {
	tests := []struct {
		name, label, want string
	}{
		{"safe label kept", "e64cd44-scan", "e64cd44-scan"},
		{"unsafe characters hashed", "tairhash@e64cd44 scan, valkey 9.1.3", "tairhash@e64cd44-scan-valkey-9.1.3-"},
		{"nothing safe", "///", "label-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := labelDir(tt.label)
			assert.True(t, strings.HasPrefix(got, tt.want), "%q -> %q", tt.label, got)
			assert.NotContains(t, got, "/")
			assert.NotContains(t, got, " ")
		})
	}
	assert.NotEqual(t, labelDir("a b"), labelDir("a,b"), "labels that sanitize alike stay distinct")
}

func TestProfilerArgvKeepsSubstitutedPathWhole(t *testing.T) {
	p := &Profiler{Template: "sample {pid} {secs} 1 -file {out}"}
	argv, err := p.argv(42, 10*time.Second, "/tmp/run dir, with spaces/server.sample.txt")
	require.NoError(t, err)
	assert.Equal(t, []string{"sample", "42", "10", "1", "-file", "/tmp/run dir, with spaces/server.sample.txt"}, argv)

	_, err = (&Profiler{Template: "  "}).argv(1, time.Second, "x")
	assert.Error(t, err)
}

func TestReclaimWorkExcludesHarnessAndIdle(t *testing.T) {
	// 20s window, 0.142s busy, 0.002s of it INFO polling, idle background 0.6%:
	// 0.142 - 0.002 - 0.12 = 0.020s of reclaim work over 20000 fields.
	m := ServerMetrics{WindowSec: 20, BusyMainSec: 0.142, HarnessSec: 0.002}
	assert.InDelta(t, 1.0, reclaimWorkUsPerField(m, 0.006, 20000), 1e-9)
	assert.Equal(t, 0.0, reclaimWorkUsPerField(m, 0.006, 0))
}

func TestParseBackendSpecs(t *testing.T) {
	tests := []struct {
		name    string
		list    string
		want    []BackendSpec
		wantErr string
	}{
		{"types name themselves", "module, lua,native", []BackendSpec{
			{Name: "module", Type: "module"}, {Name: "lua", Type: "lua"}, {Name: "native", Type: "native"},
		}, ""},
		{"named builds of one type", "develop=module@127.0.0.1:26380,lazy-index=module@127.0.0.1:26381,lua@127.0.0.1:26379", []BackendSpec{
			{Name: "develop", Type: "module", Addr: "127.0.0.1:26380"},
			{Name: "lazy-index", Type: "module", Addr: "127.0.0.1:26381"},
			{Name: "lua", Type: "lua", Addr: "127.0.0.1:26379"},
		}, ""},
		{"duplicate name", "module@a:1,module@b:2", nil, "name given twice"},
		{"unknown type", "x=redis", nil, "unknown type"},
		{"unsafe name", "a/b=module", nil, "must match"},
		{"upper-case name", "Dev=module", nil, "must match"},
		{"empty address", "module@", nil, "empty address"},
		{"empty list", " , ", nil, "no backends"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBackendSpecs(tt.list)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveReference(t *testing.T) {
	assert.Equal(t, "develop", resolveReference("develop", []string{"lua", "develop", "module"}))
	assert.Equal(t, "module", resolveReference("absent", []string{"lua", "module"}))
	assert.Equal(t, "lua", resolveReference("", []string{"lua", "native"}))
	assert.Equal(t, "", resolveReference("", nil))
}

func TestReportComparesNamedBuildsAgainstReference(t *testing.T) {
	run := func(backend string, repeat int, busy, ops, foreign float64) Result {
		return Result{
			Workload: "get", Concurrency: 128, Backend: backend, BackendType: "module", Repeat: repeat, OpsPerSec: ops,
			Server: ServerMetrics{BusyUsPerOp: busy}, Interference: Interference{WindowSec: 10, ForeignCores: foreign},
		}
	}
	// Run order, not name order, sets the row order: develop, lazy, lua.
	loads := []Result{
		run("develop", 0, 2, 100e3, 0), run("lazy", 0, 1, 200e3, 0), run("lua", 0, 4, 50e3, 0),
		run("develop", 1, 2, 100e3, 0), run("lazy", 1, 1, 200e3, 2.5), run("lua", 1, 4, 50e3, 0),
	}
	var out strings.Builder
	require.NoError(t, Report(&out, Results{Loads: loads}, ReportOptions{Reference: "develop"}))
	text := out.String()
	assert.Contains(t, text, "## summary — every backend against develop")
	assert.Contains(t, text, "| develop | 2.00 |\n| lazy | 1.00 (0.50x) † |\n| lua | 4.00 (2.00x) |")
	assert.Contains(t, text, "| lazy | 200k (2.00x) † |")
	assert.Contains(t, text, "ops/s vs develop (paired)")
	assert.Contains(t, text, "| 128 | lazy | 1.00 | 0.50x (±0%) |")
	assert.Contains(t, text, "1 of 6 windows above 1 core")
}

func TestForeignCPU(t *testing.T) {
	const self, server = 100, 200
	start := map[int]procStat{
		self:   {ppid: 1, cpu: 50, name: "thbench"},
		server: {ppid: 1, cpu: 900, name: "valkey-server"},
		300:    {ppid: 1, cpu: 10, name: "docker"},
		400:    {ppid: 1, cpu: 5, name: "old"},
		500:    {ppid: 1, cpu: 7, name: "idle"},
	}
	end := map[int]procStat{
		self:   {ppid: 1, cpu: 80, name: "thbench"},
		server: {ppid: 1, cpu: 910, name: "valkey-server"},
		300:    {ppid: 1, cpu: 12.5, name: "docker"}, // 2.5s in the window
		400:    {ppid: 1, cpu: 0.5, name: "reused"},  // pid reused: all 0.5s count
		500:    {ppid: 1, cpu: 7, name: "idle"},      // nothing used
		600:    {ppid: 1, cpu: 1, name: "compiler"},  // started inside the window
		700:    {ppid: self, cpu: 0.2, name: "ps"},   // our own child
	}
	in := foreignCPU(start, end, map[int]bool{self: true, server: true}, self, 5*time.Second)
	assert.InDelta(t, 4.0, in.ForeignCPUSec, 1e-9)
	assert.InDelta(t, 0.8, in.ForeignCores, 1e-9)
	require.Len(t, in.Top, 3)
	assert.Equal(t, ProcessCPU{PID: 300, Name: "docker", CPUSec: 2.5}, in.Top[0])
	assert.Equal(t, "compiler", in.Top[1].Name)
}

func TestParseCPUTime(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"0:00.12", 0.12},
		{"527:12.79", 527*60 + 12.79},
		{"1:02:03", 3723},
		{"2-01:00:00", 2*86400 + 3600},
	}
	for _, tt := range tests {
		got, err := parseCPUTime(tt.in)
		require.NoError(t, err, tt.in)
		assert.InDelta(t, tt.want, got, 1e-9, tt.in)
	}
	_, err := parseCPUTime("1:xx")
	assert.Error(t, err)
}

func TestParsePS(t *testing.T) {
	out := "    1     0 145:40.47 /sbin/launchd\n  659     1  0:01.50 /Applications/Some App.app/Contents/MacOS/Some App\n"
	procs, err := parsePS([]byte(out))
	require.NoError(t, err)
	assert.Equal(t, procStat{ppid: 1, cpu: 1.5, name: "Some App"}, procs[659])
	assert.InDelta(t, 145*60+40.47, procs[1].cpu, 1e-9)

	_, err = parsePS([]byte("abc 1 0:01.00 x\n"))
	assert.Error(t, err)
}

func TestParseProcStat(t *testing.T) {
	st, ok := parseProcStat([]byte("1234 (my (odd) proc) S 77 1234 1234 0 -1 4194304 100 0 0 0 250 50 0 0 20 0 1 0"))
	require.True(t, ok)
	assert.Equal(t, procStat{ppid: 77, cpu: 3, name: "my (odd) proc"}, st)
	_, ok = parseProcStat([]byte("1234 (short) S 1"))
	assert.False(t, ok)
}

func TestReadSourceRevision(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		return p
	}
	rev, err := readSourceRevision(write("clean", "7623ee5abc\n"))
	require.NoError(t, err)
	assert.Equal(t, "7623ee5abc", rev)
	rev, err = readSourceRevision(write("dirty", "7623ee5abc\n M src/tairhash.c\n"))
	require.NoError(t, err)
	assert.Equal(t, "7623ee5abc+dirty", rev)
	rev, err = readSourceRevision(filepath.Join(dir, "absent"))
	require.NoError(t, err)
	assert.Equal(t, "", rev)
}

func TestWorkloadSupports(t *testing.T) {
	versioned := Backend{Name: "module", Versioned: fakeVersioned{}}
	unversioned := Backend{Name: "native"}
	plain := Backend{Name: "hash", NoTTL: true}
	tests := []struct {
		workload string
		backend  Backend
		want     bool
	}{
		{"set-ttl", versioned, true},
		{"set-ttl", plain, false},
		{"set-nottl", plain, true},
		{"getall-nottl", plain, true},
		{"incr-ttl", plain, false},
		{"cas", versioned, true},
		{"cas", unversioned, false},
		{"cas", plain, false},
	}
	for _, tt := range tests {
		t.Run(tt.workload+"/"+tt.backend.Name, func(t *testing.T) {
			wl, ok := LookupWorkload(tt.workload)
			require.True(t, ok)
			assert.Equal(t, tt.want, wl.Supports(tt.backend))
		})
	}
}

// fakeVersioned only marks a Backend as versioned; Supports never calls it.
type fakeVersioned struct{ fieldhash.VersionedStore }

func TestWorkloadNamesPairTTLVariants(t *testing.T) {
	names := WorkloadNames()
	for _, n := range names {
		base, ok := strings.CutSuffix(n, "-nottl")
		if !ok {
			continue
		}
		twin := base
		if base == "set" || base == "incr" {
			twin = base + "-ttl"
		}
		assert.Contains(t, names, twin, "%s has no TTL'd counterpart", n)
	}
}

func TestReportMemoryAgainstPlainHash(t *testing.T) {
	rs := Results{Memory: []MemoryResult{
		{Label: "l", Backend: "develop", BackendType: "module", Keys: 10, Fields: 10,
			NoTTL: LoadMemory{BytesPerField: 200, Encoding: "raw"}, TTL: &LoadMemory{BytesPerField: 300, Encoding: "raw"}},
		{Label: "l", Backend: "hash", BackendType: "hash", Keys: 10, Fields: 10,
			NoTTL: LoadMemory{BytesPerField: 100, Encoding: "listpack"}},
	}}
	var out strings.Builder
	require.NoError(t, Report(&out, rs, ReportOptions{}))
	text := out.String()
	assert.Contains(t, text, "| l | develop | 100 | raw / raw | 200 | 2.00x | 300 | 3.00x | +100 (+50%) |")
	assert.Contains(t, text, "| l | hash | 100 | listpack | 100 | 1.00x | — | — | — |")
}

func TestResultsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.jsonl")
	w, err := OpenWriter(path)
	require.NoError(t, err)
	require.NoError(t, w.Write(Result{Kind: "load", Backend: "module"}))
	require.NoError(t, w.Write(ReclaimResult{Kind: "reclaim", Backend: "lua", LoadMemory: LoadMemory{BytesPerField: 148}}))
	require.NoError(t, w.Write(MemoryResult{Kind: "memory", Backend: "hash", NoTTL: LoadMemory{BytesPerField: 90}}))
	require.NoError(t, w.Close())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"bytes_per_field":148`, "reclaim keeps its load fields at the top level")

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	var rs Results
	require.NoError(t, rs.ReadResults(f))
	require.Len(t, rs.Loads, 1)
	require.Len(t, rs.Reclaims, 1)
	require.Len(t, rs.Memory, 1)
	assert.Equal(t, 148.0, rs.Reclaims[0].BytesPerField)
	assert.Equal(t, 90.0, rs.Memory[0].NoTTL.BytesPerField)
	assert.Error(t, (&Results{}).ReadResults(strings.NewReader(`{"kind":"other"}`)))
}
