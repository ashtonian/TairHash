package bench

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/ashtonian/TairHash/go/internal/serverprof"
)

// ReferenceBackend is the baseline other backends are compared against.
const ReferenceBackend = "module"

// harnessCommands are issued by the bench itself (snapshots, timeline, resets)
// and are left out of per-command tables.
var harnessCommands = map[string]bool{
	"info": true, "dbsize": true, "config|resetstat": true, "config|get": true, "slowlog|reset": true,
	"slowlog|get": true, "latency|reset": true, "latency|latest": true, "latency|history": true,
	"module|list": true, "exhexpireinfo": true, "memory|stats": true, "memory|usage": true, "ping": true,
	"hello": true, "client|setinfo": true, "select": true, "function|load": true, "flushdb": true,
	"command|info": true, "exhlen": true,
}

type cell struct {
	workload    string
	concurrency int
	backend     string
}

type aggregate struct {
	runs     []Result
	profiles []Result
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func spread(xs []float64) float64 {
	m := median(xs)
	if m == 0 || len(xs) < 2 {
		return 0
	}
	return (slices.Max(xs) - slices.Min(xs)) / m
}

func (a *aggregate) med(f func(Result) float64) float64 {
	xs := make([]float64, len(a.runs))
	for i, r := range a.runs {
		xs[i] = f(r)
	}
	return median(xs)
}

func (a *aggregate) all(f func(Result) float64) []float64 {
	xs := make([]float64, len(a.runs))
	for i, r := range a.runs {
		xs[i] = f(r)
	}
	return xs
}

func backendRank(name string) int {
	switch name {
	case "module":
		return 0
	case "lua":
		return 1
	case "native":
		return 2
	}
	return 3
}

// Report renders results as Markdown: medians across measured repeats per
// workload, concurrency and backend, the server's own cost and command mix, and
// the main-thread profile breakdown from the profiled pass.
func Report(w io.Writer, loads []Result, reclaims []ReclaimResult) error {
	var b strings.Builder
	writeEnvironment(&b, loads, reclaims)

	aggs := map[cell]*aggregate{}
	var workloads []string
	for _, r := range loads {
		c := cell{r.Workload, r.Concurrency, r.Backend}
		a := aggs[c]
		if a == nil {
			a = &aggregate{}
			aggs[c] = a
		}
		if r.Profiled {
			a.profiles = append(a.profiles, r)
		} else {
			a.runs = append(a.runs, r)
		}
		if !slices.Contains(workloads, r.Workload) {
			workloads = append(workloads, r.Workload)
		}
	}
	order := map[string]int{}
	for i, wl := range Workloads {
		order[wl.Name] = i
	}
	sort.SliceStable(workloads, func(i, j int) bool { return order[workloads[i]] < order[workloads[j]] })

	for _, wl := range workloads {
		var cells []cell
		for c := range aggs {
			if c.workload == wl {
				cells = append(cells, c)
			}
		}
		sort.Slice(cells, func(i, j int) bool {
			if cells[i].concurrency != cells[j].concurrency {
				return cells[i].concurrency < cells[j].concurrency
			}
			return backendRank(cells[i].backend) < backendRank(cells[j].backend)
		})
		desc := ""
		if w, ok := LookupWorkload(wl); ok {
			desc = " — " + w.Description
		}
		fmt.Fprintf(&b, "\n## %s%s\n", wl, desc)
		writeClientTable(&b, cells, aggs)
		writeServerTable(&b, cells, aggs)
		writeCommandTable(&b, cells, aggs)
		writeProfileTable(&b, cells, aggs)
		if wl == "cas" {
			b.WriteString("\nConflicts per successful update (median): ")
			var parts []string
			for _, c := range cells {
				a := aggs[c]
				parts = append(parts, fmt.Sprintf("%s c=%d %.4f", c.backend, c.concurrency, a.med(func(r Result) float64 {
					if r.Ops == 0 {
						return 0
					}
					return float64(r.Conflicts) / float64(r.Ops)
				})))
			}
			b.WriteString(strings.Join(parts, "; ") + "\n")
		}
	}
	writeReclaim(&b, reclaims)
	_, err := io.WriteString(w, b.String())
	return err
}

func ratio(a, ref *aggregate, f func(Result) float64) string {
	if ref == nil || len(ref.runs) == 0 || len(a.runs) == 0 {
		return "—"
	}
	r := ref.med(f)
	if r == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2fx", a.med(f)/r)
}

func writeClientTable(b *strings.Builder, cells []cell, aggs map[cell]*aggregate) {
	b.WriteString("\n**Client** (median of measured runs)\n\n")
	b.WriteString("| conc | backend | runs | ops/s (±spread) | p50 µs | p99 µs | p99.9 µs | ops/s vs module |\n")
	b.WriteString("|---:|---|---:|---:|---:|---:|---:|---:|\n")
	for _, c := range cells {
		a := aggs[c]
		if len(a.runs) == 0 {
			continue
		}
		ref := aggs[cell{c.workload, c.concurrency, ReferenceBackend}]
		ops := func(r Result) float64 { return r.OpsPerSec }
		fmt.Fprintf(b, "| %d | %s | %d | %.0f (±%.0f%%) | %.1f | %.1f | %.1f | %s |\n",
			c.concurrency, c.backend, len(a.runs), a.med(ops), 50*spread(a.all(ops)),
			a.med(func(r Result) float64 { return r.Latency.P50 }),
			a.med(func(r Result) float64 { return r.Latency.P99 }),
			a.med(func(r Result) float64 { return r.Latency.P999 }),
			ratio(a, ref, ops))
	}
}

func writeServerTable(b *strings.Builder, cells []cell, aggs map[cell]*aggregate) {
	b.WriteString("\n**Server cost per op** (median; event loop split into command execution and everything else)\n\n")
	b.WriteString("| conc | backend | active µs | vs module | CPU µs | util (timeline min–max) | loop µs | cmd µs | other µs | ops/cycle | reads/op | writes/op | B in | B out | cmds/op | slowlog | errors |\n")
	b.WriteString("|---:|---|---:|---:|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, c := range cells {
		a := aggs[c]
		if len(a.runs) == 0 {
			continue
		}
		ref := aggs[cell{c.workload, c.concurrency, ReferenceBackend}]
		active := func(r Result) float64 { return r.Server.ActiveUsPerOp }
		var tlMin, tlMax []float64
		var slow, errs int64
		for _, r := range a.runs {
			tlMin = append(tlMin, r.Server.Timeline.UtilizationMin)
			tlMax = append(tlMax, r.Server.Timeline.UtilizationMax)
			slow += int64(r.Server.Slowlog.Entries)
			errs += r.Server.ErrorReplies
		}
		fmt.Fprintf(b, "| %d | %s | %.2f | %s | %.2f | %.2f (%.2f–%.2f) | %.2f | %.2f | %.2f | %.1f | %.3f | %.3f | %.0f | %.0f | %.2f | %d | %d |\n",
			c.concurrency, c.backend, a.med(active), ratio(a, ref, active),
			a.med(func(r Result) float64 { return r.Server.CPUUsPerOp }),
			a.med(func(r Result) float64 { return r.Server.Utilization }), median(tlMin), median(tlMax),
			a.med(func(r Result) float64 { return r.Server.EventLoop.UsPerOp }),
			a.med(func(r Result) float64 { return r.Server.EventLoop.CmdUsPerOp }),
			a.med(func(r Result) float64 { return r.Server.EventLoop.NonCmdUsPerOp }),
			a.med(func(r Result) float64 { return r.Server.EventLoop.OpsPerCycle }),
			a.med(func(r Result) float64 { return r.Server.IO.ReadsPerOp }),
			a.med(func(r Result) float64 { return r.Server.IO.WritesPerOp }),
			a.med(func(r Result) float64 { return r.Server.IO.InBytesPerOp }),
			a.med(func(r Result) float64 { return r.Server.IO.OutBytesPerOp }),
			a.med(func(r Result) float64 { return r.Server.CommandsPerOp }),
			slow, errs)
	}
}

func writeCommandTable(b *strings.Builder, cells []cell, aggs map[cell]*aggregate) {
	b.WriteString("\n**Server commands** (median; latency is server-side execution from INFO latencystats; script-issued commands included)\n\n")
	b.WriteString("| conc | backend | command | calls/op | µs/call | µs/op | p50 µs | p99 µs | p99.9 µs |\n")
	b.WriteString("|---:|---|---|---:|---:|---:|---:|---:|---:|\n")
	for _, c := range cells {
		a := aggs[c]
		if len(a.runs) == 0 {
			continue
		}
		usPerOp := map[string]float64{}
		for name, cm := range a.runs[0].Server.PerCommand {
			if !harnessCommands[name] {
				usPerOp[name] = cm.CallsPerOp * cm.UsecPerCall
			}
		}
		names := make([]string, 0, len(usPerOp))
		for n := range usPerOp {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool { return usPerOp[names[i]] > usPerOp[names[j]] })
		for _, name := range names {
			cmd := func(f func(CommandMetrics) float64) float64 {
				return a.med(func(r Result) float64 { return f(r.Server.PerCommand[name]) })
			}
			fmt.Fprintf(b, "| %d | %s | %s | %.3f | %.2f | %.2f | %.2f | %.2f | %.2f |\n",
				c.concurrency, c.backend, name,
				cmd(func(m CommandMetrics) float64 { return m.CallsPerOp }),
				cmd(func(m CommandMetrics) float64 { return m.UsecPerCall }),
				cmd(func(m CommandMetrics) float64 { return m.CallsPerOp * m.UsecPerCall }),
				cmd(func(m CommandMetrics) float64 { return m.Latency["p50"] }),
				cmd(func(m CommandMetrics) float64 { return m.Latency["p99"] }),
				cmd(func(m CommandMetrics) float64 { return m.Latency["p99.9"] }))
		}
	}
}

func writeProfileTable(b *strings.Builder, cells []cell, aggs map[cell]*aggregate) {
	var rows []string
	for _, c := range cells {
		for _, r := range aggs[c].profiles {
			if r.ServerProfile == nil {
				continue
			}
			s := r.ServerProfile.Summary
			rows = append(rows, fmt.Sprintf("| %d | %s | %.1f | %s | %s | %s | `%s` |",
				c.concurrency, c.backend, s.BusyPercent,
				formatShares(s.Phases, 6), formatShares(s.Components, 7), formatShares(s.TopSelf, 4),
				relativeToRuns(r.ServerProfile.Outputs.MainPprof)))
		}
	}
	if len(rows) == 0 {
		return
	}
	b.WriteString("\n**Server main-thread profile** (profiled pass; % of busy samples)\n\n")
	b.WriteString("| conc | backend | busy % | phases | components | top self | pprof |\n")
	b.WriteString("|---:|---|---:|---|---|---|---|\n")
	b.WriteString(strings.Join(rows, "\n") + "\n")
}

// relativeToRuns shortens an artifact path to "<run label>/<file>".
func relativeToRuns(path string) string {
	return filepath.Join(filepath.Base(filepath.Dir(path)), filepath.Base(path))
}

func formatShares(shares []serverprof.Share, n int) string {
	parts := make([]string, 0, n)
	for i, sh := range shares {
		if i == n {
			break
		}
		name := sh.Name
		if i := strings.Index(name, " ("); i > 0 {
			name = name[:i]
		}
		parts = append(parts, fmt.Sprintf("%s %.0f", name, sh.Percent))
	}
	return strings.Join(parts, ", ")
}

func writeReclaim(b *strings.Builder, reclaims []ReclaimResult) {
	if len(reclaims) == 0 {
		return
	}
	b.WriteString("\n## reclaim — every field expires at one deadline; nothing reads them afterwards\n\n")
	b.WriteString("| label | backend | fields | used_memory B/field | dataset B/field | MEMORY USAGE B/field | load s | reclaimed | reclaim s | fields/s | active s | active µs/field | server expired_fields | module active/passive | sweeps | latency events |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|---|---:|---:|---:|---:|---:|---|---:|---|\n")
	for _, r := range reclaims {
		reclaimed := "yes"
		if !r.Reclaimed {
			reclaimed = fmt.Sprintf("no (%d keys left)", r.RemainingKeys)
		}
		module := "—"
		if r.Server.ModuleActiveExpired+r.Server.ModulePassiveExpired > 0 {
			module = fmt.Sprintf("%d / %d", r.Server.ModuleActiveExpired, r.Server.ModulePassiveExpired)
		}
		var events []string
		for _, e := range r.Server.LatencyEvents {
			events = append(events, fmt.Sprintf("%s max %dms", e.Event, e.MaxMs))
		}
		fmt.Fprintf(b, "| %s | %s | %d | %.0f | %.0f | %.0f | %.1f | %s | %.1f | %.0f | %.3f | %.2f | %d | %s | %d | %s |\n",
			r.Label, r.Backend, r.Keys*r.Fields, r.BytesPerField, r.DatasetBytesPerField, r.MemoryUsagePerField,
			r.LoadSec, reclaimed, r.ReclaimSec, r.FieldsPerSec, r.Server.ActiveMainSec, r.Server.ActiveUsPerOp,
			r.Server.ExpiredFields, module, r.SweepCalls, strings.Join(events, ", "))
	}
}

func writeEnvironment(b *strings.Builder, loads []Result, reclaims []ReclaimResult) {
	type server struct {
		facts Facts
		host  HostFacts
	}
	var servers []server
	seen := map[string]bool{}
	add := func(f Facts, h HostFacts) {
		key := fmt.Sprintf("%s|%d|%v", f.Executable, f.ProcessID, f.Modules)
		if !seen[key] {
			seen[key] = true
			servers = append(servers, server{f, h})
		}
	}
	for _, r := range loads {
		add(r.ServerFacts, r.Host)
	}
	for _, r := range reclaims {
		add(r.ServerFacts, r.Host)
	}
	if len(servers) == 0 {
		b.WriteString("No results.\n")
		return
	}
	b.WriteString("## environment\n\n")
	for _, s := range servers {
		f := s.facts
		fmt.Fprintf(b, "- server pid %d: valkey %s (%s, %s), modules [%s]; io-threads=%s hz=%s latency-tracking=%s (%s) slowlog>%sµs latency-monitor>%sms\n",
			f.ProcessID, f.ValkeyVersion, f.Allocator, f.Multiplexing, strings.Join(f.Modules, ", "),
			f.Config["io-threads"], f.Config["hz"], f.Config["latency-tracking"], f.Config["latency-tracking-info-percentiles"],
			f.Config["slowlog-log-slower-than"], f.Config["latency-monitor-threshold"])
		if len(f.ModuleExpire) > 0 {
			keys := make([]string, 0, len(f.ModuleExpire))
			for k, v := range f.ModuleExpire {
				keys = append(keys, strings.TrimPrefix(k, "tair_hash_")+"="+v)
			}
			sort.Strings(keys)
			fmt.Fprintf(b, "  - tairhash: %s\n", strings.Join(keys, " "))
		}
	}
	h := servers[0].host
	fmt.Fprintf(b, "- client %s %s/%s, %d CPUs, GOMAXPROCS=%d\n", h.GoVersion, h.GOOS, h.GOARCH, h.NumCPU, h.GOMAXPROCS)
	var la []float64
	for _, r := range loads {
		la = append(la, r.LoadAvg[0], r.LoadAvg[1])
	}
	if len(la) > 0 {
		fmt.Fprintf(b, "- host 1-minute load average during runs: %.2f–%.2f\n", slices.Min(la), slices.Max(la))
	}
}
