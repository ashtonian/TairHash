package bench

import (
	"fmt"
	"strconv"
)

// ServerMetrics is the server's own account of a measured window, derived from
// INFO snapshots taken at its start and end. Per-op figures divide by the
// logical operations the client completed (for reclaim: by fields reclaimed).
type ServerMetrics struct {
	WindowSec float64 `json:"window_sec"`

	// BusyMainSec is Δeventloop_duration_sum: the processing time of every
	// event-loop iteration, from wakeup to the next sleep. Every command runs on
	// the main thread, so it bounds throughput. used_active_time_main_thread is
	// not used: it skips iterations without client I/O, which is where module
	// timers and cron-driven expiry do their work.
	BusyMainSec float64 `json:"busy_main_sec"`
	// HarnessSec is the execution time of the bench's own commands in the
	// window (snapshots, polling), from commandstats. BusyUsPerOp excludes it.
	HarnessSec float64 `json:"harness_sec"`
	CPUUserSec float64 `json:"cpu_user_sec"`
	CPUSysSec  float64 `json:"cpu_sys_sec"`
	// Utilization is BusyMainSec over the window; near 1 the server was the
	// bottleneck, well below it the client or the round trip was.
	Utilization float64 `json:"utilization"`
	// BusyUsPerOp is main-thread time per operation net of harness commands.
	BusyUsPerOp float64 `json:"busy_us_per_op"`
	CPUUsPerOp  float64 `json:"cpu_us_per_op"`

	EventLoop EventLoopMetrics `json:"eventloop"`
	IO        IOMetrics        `json:"io"`

	Commands      int64   `json:"commands"`
	CommandsPerOp float64 `json:"commands_per_op"`
	// PerCommand covers every command executed in the window, including those
	// issued by scripts and module timers, with the server-side latency
	// percentiles from INFO latencystats.
	PerCommand   map[string]CommandMetrics `json:"per_command"`
	Errors       map[string]int64          `json:"errors,omitempty"`
	ErrorReplies int64                     `json:"error_replies"`

	ExpiredKeys            int64 `json:"expired_keys"`
	ExpiredFields          int64 `json:"expired_fields"`
	ExpireTimeCapReached   int64 `json:"expire_time_cap_reached"`
	ModuleActiveExpired    int64 `json:"module_active_expired_fields,omitempty"`
	ModulePassiveExpired   int64 `json:"module_passive_expired_fields,omitempty"`
	ModuleActiveExpireLast int64 `json:"module_active_expire_last_ms,omitempty"`
	ModuleActiveExpireMax  int64 `json:"module_active_expire_max_ms,omitempty"`

	Memory   MemoryMetrics `json:"memory"`
	Keyspace string        `json:"keyspace"`
	Clients  int64         `json:"connected_clients"`

	Slowlog       SlowlogSummary `json:"slowlog"`
	LatencyEvents []LatencyEvent `json:"latency_events,omitempty"`
	Timeline      TimelineStats  `json:"timeline"`
}

// EventLoopMetrics splits event-loop time into command execution and the rest
// (reading, parsing, replying, before/after-sleep hooks, timers).
type EventLoopMetrics struct {
	Cycles     int64 `json:"cycles"`
	DurationUs int64 `json:"duration_us"`
	CmdUs      int64 `json:"cmd_us"`
	CronUs     int64 `json:"cron_us"`
	// UsPerOp = DurationUs/ops, CmdUsPerOp = CmdUs/ops, NonCmdUsPerOp the difference.
	UsPerOp       float64 `json:"us_per_op"`
	CmdUsPerOp    float64 `json:"cmd_us_per_op"`
	NonCmdUsPerOp float64 `json:"noncmd_us_per_op"`
	// OpsPerCycle is the batching factor: logical operations served per loop
	// iteration. Higher means syscalls and wakeups are amortised over more work.
	OpsPerCycle float64 `json:"ops_per_cycle"`
	// MaxCycleUs and MaxCmdsPerCycle are server-lifetime maxima from INFO debug.
	MaxCycleUs      int64 `json:"max_cycle_us"`
	MaxCmdsPerCycle int64 `json:"max_cmds_per_cycle"`
}

// IOMetrics counts socket reads/writes and bytes.
type IOMetrics struct {
	Reads         int64   `json:"reads"`
	Writes        int64   `json:"writes"`
	ReadsPerOp    float64 `json:"reads_per_op"`
	WritesPerOp   float64 `json:"writes_per_op"`
	NetInBytes    int64   `json:"net_in_bytes"`
	NetOutBytes   int64   `json:"net_out_bytes"`
	InBytesPerOp  float64 `json:"in_bytes_per_op"`
	OutBytesPerOp float64 `json:"out_bytes_per_op"`
	ThreadedReads int64   `json:"io_threaded_reads"`
}

// CommandMetrics is one command's share of the window.
type CommandMetrics struct {
	Calls       int64   `json:"calls"`
	Usec        int64   `json:"usec"`
	UsecPerCall float64 `json:"usec_per_call"`
	CallsPerOp  float64 `json:"calls_per_op"`
	Rejected    int64   `json:"rejected_calls,omitempty"`
	Failed      int64   `json:"failed_calls,omitempty"`
	// Latency holds server-side execution-time percentiles in µs ("p50" ...).
	Latency map[string]float64 `json:"latency_us,omitempty"`
}

// MemoryMetrics is the memory state at the end of the window plus the change.
type MemoryMetrics struct {
	UsedMemory      int64   `json:"used_memory"`
	UsedMemoryDelta int64   `json:"used_memory_delta"`
	Dataset         int64   `json:"dataset"`
	DatasetDelta    int64   `json:"dataset_delta"`
	Overhead        int64   `json:"overhead"`
	VMTotal         int64   `json:"vm_total"`
	VMFunctions     int64   `json:"vm_functions"`
	VMEval          int64   `json:"vm_eval"`
	Scripts         int64   `json:"scripts"`
	FragRatio       float64 `json:"fragmentation_ratio"`
	RSS             int64   `json:"rss"`
	Peak            int64   `json:"peak"`
}

// SlowlogSummary condenses the SLOWLOG entries of the window.
type SlowlogSummary struct {
	Entries   int            `json:"entries"`
	MaxUs     int64          `json:"max_us"`
	ByCommand map[string]int `json:"by_command,omitempty"`
}

// TimelineStats summarises the per-second samples taken during the window.
type TimelineStats struct {
	Samples         int     `json:"samples"`
	UtilizationMin  float64 `json:"utilization_min"`
	UtilizationMax  float64 `json:"utilization_max"`
	UtilizationMean float64 `json:"utilization_mean"`
	CmdRateMin      float64 `json:"cmd_rate_min"`
	CmdRateMax      float64 `json:"cmd_rate_max"`
}

// harnessCommands are issued by the bench itself (snapshots, timeline, polling,
// resets). Their execution time is subtracted from per-op cost and they are
// left out of per-command tables.
var harnessCommands = map[string]bool{
	"info": true, "dbsize": true, "config|resetstat": true, "config|get": true, "slowlog|reset": true,
	"slowlog|get": true, "latency|reset": true, "latency|latest": true, "latency|history": true,
	"module|list": true, "exhexpireinfo": true, "memory|stats": true, "memory|usage": true, "object|encoding": true, "ping": true,
	"hello": true, "client|setinfo": true, "select": true, "function|load": true, "flushdb": true,
	"command|info": true, "exhlen": true,
}

// busyRate is main-thread utilization between two INFO samples dt seconds apart.
func busyRate(prev, cur map[string]string, dt float64) float64 {
	return (infoFloat(cur, "eventloop_duration_sum") - infoFloat(prev, "eventloop_duration_sum")) / 1e6 / dt
}

// Measure derives ServerMetrics from snapshots bracketing a window of ops
// operations on database db. before must follow ResetDiagnostics so the
// commandstats and latencystats in after cover only the window.
func Measure(before, after Snapshot, ops int64, db int) (ServerMetrics, error) {
	window := after.At.Sub(before.At)
	m := ServerMetrics{
		WindowSec:     window.Seconds(),
		BusyMainSec:   float64(after.int("eventloop_duration_sum")-before.int("eventloop_duration_sum")) / 1e6,
		CPUUserSec:    after.float("used_cpu_user") - before.float("used_cpu_user"),
		CPUSysSec:     after.float("used_cpu_sys") - before.float("used_cpu_sys"),
		Commands:      after.int("total_commands_processed") - before.int("total_commands_processed"),
		ErrorReplies:  after.int("total_error_replies") - before.int("total_error_replies"),
		ExpiredKeys:   after.int("expired_keys") - before.int("expired_keys"),
		ExpiredFields: after.int("expired_fields") - before.int("expired_fields"),
		ExpireTimeCapReached: after.int("expired_time_cap_reached_count") -
			before.int("expired_time_cap_reached_count"),
		Keyspace: after.Info["db"+strconv.Itoa(db)],
		Clients:  after.int("connected_clients"),
		Errors:   after.errorStats(),
	}
	if window > 0 {
		m.Utilization = m.BusyMainSec / window.Seconds()
	}
	m.EventLoop = EventLoopMetrics{
		Cycles:          after.int("eventloop_cycles") - before.int("eventloop_cycles"),
		DurationUs:      after.int("eventloop_duration_sum") - before.int("eventloop_duration_sum"),
		CmdUs:           after.int("eventloop_duration_cmd_sum") - before.int("eventloop_duration_cmd_sum"),
		CronUs:          after.int("eventloop_duration_cron_sum") - before.int("eventloop_duration_cron_sum"),
		MaxCycleUs:      after.int("eventloop_duration_max"),
		MaxCmdsPerCycle: after.int("eventloop_cmd_per_cycle_max"),
	}
	m.IO = IOMetrics{
		Reads:         after.int("total_reads_processed") - before.int("total_reads_processed"),
		Writes:        after.int("total_writes_processed") - before.int("total_writes_processed"),
		NetInBytes:    after.int("total_net_input_bytes") - before.int("total_net_input_bytes"),
		NetOutBytes:   after.int("total_net_output_bytes") - before.int("total_net_output_bytes"),
		ThreadedReads: after.int("io_threaded_reads_processed") - before.int("io_threaded_reads_processed"),
	}
	m.Memory = MemoryMetrics{
		UsedMemory:      after.int("used_memory"),
		UsedMemoryDelta: after.int("used_memory") - before.int("used_memory"),
		Dataset:         after.int("used_memory_dataset"),
		DatasetDelta:    after.int("used_memory_dataset") - before.int("used_memory_dataset"),
		Overhead:        after.int("used_memory_overhead"),
		VMTotal:         after.int("used_memory_vm_total"),
		VMFunctions:     after.int("used_memory_vm_functions"),
		VMEval:          after.int("used_memory_vm_eval"),
		Scripts:         after.int("used_memory_scripts"),
		FragRatio:       after.float("mem_fragmentation_ratio"),
		RSS:             after.int("used_memory_rss"),
		Peak:            after.int("used_memory_peak"),
	}

	stats, err := after.cmdStats()
	if err != nil {
		return ServerMetrics{}, err
	}
	latency := after.latencyPercentiles()
	m.PerCommand = make(map[string]CommandMetrics, len(stats))
	for name, st := range stats {
		cm := CommandMetrics{Calls: st.Calls, Usec: st.Usec, Rejected: st.Rejected, Failed: st.Failed, Latency: latency[name]}
		if st.Calls > 0 {
			cm.UsecPerCall = float64(st.Usec) / float64(st.Calls)
		}
		if ops > 0 {
			cm.CallsPerOp = float64(st.Calls) / float64(ops)
		}
		m.PerCommand[name] = cm
		if harnessCommands[name] {
			m.HarnessSec += float64(st.Usec) / 1e6
		}
	}

	if a1, p1, ok := moduleExpireCounts(after.ModuleExpire, db); ok {
		a0, p0, _ := moduleExpireCounts(before.ModuleExpire, db)
		m.ModuleActiveExpired, m.ModulePassiveExpired = a1-a0, p1-p0
		info := parseInfoText(after.ModuleExpire)
		m.ModuleActiveExpireLast, _ = strconv.ParseInt(info["tair_hash_active_expire_last_time_msec"], 10, 64)
		m.ModuleActiveExpireMax, _ = strconv.ParseInt(info["tair_hash_active_expire_max_time_msec"], 10, 64)
	}

	if ops > 0 {
		n := float64(ops)
		m.BusyUsPerOp = (m.BusyMainSec - m.HarnessSec) * 1e6 / n
		m.CPUUsPerOp = (m.CPUUserSec + m.CPUSysSec) * 1e6 / n
		m.CommandsPerOp = float64(m.Commands) / n
		m.EventLoop.UsPerOp = float64(m.EventLoop.DurationUs) / n
		m.EventLoop.CmdUsPerOp = float64(m.EventLoop.CmdUs) / n
		m.EventLoop.NonCmdUsPerOp = m.EventLoop.UsPerOp - m.EventLoop.CmdUsPerOp
		m.IO.ReadsPerOp = float64(m.IO.Reads) / n
		m.IO.WritesPerOp = float64(m.IO.Writes) / n
		m.IO.InBytesPerOp = float64(m.IO.NetInBytes) / n
		m.IO.OutBytesPerOp = float64(m.IO.NetOutBytes) / n
	}
	if m.EventLoop.Cycles > 0 {
		m.EventLoop.OpsPerCycle = float64(ops) / float64(m.EventLoop.Cycles)
	}
	if m.WindowSec < 0 {
		return ServerMetrics{}, fmt.Errorf("snapshots out of order: %s before %s", after.At, before.At)
	}
	return m, nil
}

// summarizeSlowlog condenses entries; the command name is the first argument.
func summarizeSlowlog(entries []SlowEntry) SlowlogSummary {
	s := SlowlogSummary{Entries: len(entries), ByCommand: map[string]int{}}
	for _, e := range entries {
		if e.DurationUs > s.MaxUs {
			s.MaxUs = e.DurationUs
		}
		name := "?"
		if len(e.Args) > 0 {
			name = e.Args[0]
		}
		s.ByCommand[name]++
	}
	return s
}

// timelineStats summarises timeline samples.
func timelineStats(samples []TimelineSample) TimelineStats {
	st := TimelineStats{Samples: len(samples)}
	if len(samples) == 0 {
		return st
	}
	st.UtilizationMin, st.CmdRateMin = samples[0].Utilization, samples[0].CmdPerSec
	var sum float64
	for _, s := range samples {
		st.UtilizationMin = min(st.UtilizationMin, s.Utilization)
		st.UtilizationMax = max(st.UtilizationMax, s.Utilization)
		st.CmdRateMin = min(st.CmdRateMin, s.CmdPerSec)
		st.CmdRateMax = max(st.CmdRateMax, s.CmdPerSec)
		sum += s.Utilization
	}
	st.UtilizationMean = sum / float64(len(samples))
	return st
}
