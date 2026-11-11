package bench

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, Report(&out, loads, nil))
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
	assert.Equal(t, "0.50x (±25%)", pairedRatio(lua, module, ops))
	assert.Equal(t, "—", pairedRatio(lua, nil, ops))
	assert.Equal(t, "—", pairedRatio(runs(), module, ops))
}

func TestReportKeepsLabelsApart(t *testing.T) {
	loads := []Result{
		{Label: "build-a", Workload: "get", Concurrency: 1, Backend: "module", OpsPerSec: 100},
		{Label: "build-b", Workload: "get", Concurrency: 1, Backend: "module", OpsPerSec: 300},
	}
	var out strings.Builder
	require.NoError(t, Report(&out, loads, nil))
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
