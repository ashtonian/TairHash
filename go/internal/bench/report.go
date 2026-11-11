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

// ReportOptions shape the rendered report.
type ReportOptions struct {
	// Reference is the backend every other is compared against. Empty, or
	// absent from a label, picks "module" when the label has it, else the
	// label's first backend.
	Reference string
}

// noisyForeignCores flags a cell whose measured runs saw other processes use
// more than this many cores: enough to take a core the server or client needed.
const noisyForeignCores = 1.0

type cell struct {
	label       string
	workload    string
	concurrency int
	backend     string
}

type aggregate struct {
	runs     []Result
	profiles []Result
}

// labelView is one label's results: backends in the order they ran, which is
// the -backends order.
type labelView struct {
	name        string
	backends    []string
	reference   string
	workloads   []string
	concurrency []int
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
	return median(a.all(f))
}

func (a *aggregate) all(f func(Result) float64) []float64 {
	xs := make([]float64, len(a.runs))
	for i, r := range a.runs {
		xs[i] = f(r)
	}
	return xs
}

// noisy reports whether any measured run saw other processes use more than
// noisyForeignCores.
func (a *aggregate) noisy() bool {
	for _, r := range a.runs {
		if r.Interference.ForeignCores > noisyForeignCores {
			return true
		}
	}
	return false
}

func resolveReference(want string, backends []string) string {
	for _, c := range []string{want, "module"} {
		if c != "" && slices.Contains(backends, c) {
			return c
		}
	}
	if len(backends) == 0 {
		return ""
	}
	return backends[0]
}

// Report renders results as Markdown: per label, a summary matrix of every
// backend against the reference, then per workload the medians across measured
// repeats, the server's own cost and command mix, and the main-thread profile
// breakdown from the profiled pass. Results with different labels (builds,
// server configs) are never pooled.
func Report(w io.Writer, loads []Result, reclaims []ReclaimResult, opts ReportOptions) error {
	var b strings.Builder
	writeEnvironment(&b, loads, reclaims)

	aggs := map[cell]*aggregate{}
	var views []*labelView
	byLabel := map[string]*labelView{}
	for _, r := range loads {
		v := byLabel[r.Label]
		if v == nil {
			v = &labelView{name: r.Label}
			byLabel[r.Label] = v
			views = append(views, v)
		}
		if !slices.Contains(v.backends, r.Backend) {
			v.backends = append(v.backends, r.Backend)
		}
		if !slices.Contains(v.workloads, r.Workload) {
			v.workloads = append(v.workloads, r.Workload)
		}
		if !slices.Contains(v.concurrency, r.Concurrency) {
			v.concurrency = append(v.concurrency, r.Concurrency)
		}
		c := cell{r.Label, r.Workload, r.Concurrency, r.Backend}
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
	}
	order := map[string]int{}
	for i, wl := range Workloads {
		order[wl.Name] = i
	}
	for _, v := range views {
		v.reference = resolveReference(opts.Reference, v.backends)
		sort.SliceStable(v.workloads, func(i, j int) bool { return order[v.workloads[i]] < order[v.workloads[j]] })
		slices.Sort(v.concurrency)
		if len(views) > 1 {
			name := v.name
			if name == "" {
				name = "(no label)"
			}
			fmt.Fprintf(&b, "\n# %s\n", name)
		}
		writeSummary(&b, v, aggs)
		for _, wl := range v.workloads {
			writeWorkload(&b, v, wl, aggs)
		}
	}
	writeReclaim(&b, reclaims, opts)
	_, err := io.WriteString(w, b.String())
	return err
}

// writeSummary renders, per concurrency level, every backend's server cost and
// throughput per workload with its paired ratio to the reference.
func writeSummary(b *strings.Builder, v *labelView, aggs map[cell]*aggregate) {
	if len(v.backends) < 2 {
		return
	}
	fmt.Fprintf(b, "\n## summary — every backend against %s\n\n", v.reference)
	fmt.Fprintf(b, "Medians of measured runs; ratios are paired per repeat against %s. † marks a cell where a measured run saw other processes use more than %.0f core.\n", v.reference, noisyForeignCores)
	metrics := []struct {
		title string
		f     func(Result) float64
		fmt   func(float64) string
	}{
		{"server busy µs/op", func(r Result) float64 { return r.Server.BusyUsPerOp }, func(x float64) string { return fmt.Sprintf("%.2f", x) }},
		{"ops/s", func(r Result) float64 { return r.OpsPerSec }, func(x float64) string { return fmt.Sprintf("%.0fk", x/1e3) }},
	}
	for _, conc := range v.concurrency {
		for _, m := range metrics {
			fmt.Fprintf(b, "\n**%s, concurrency %d**\n\n", m.title, conc)
			b.WriteString("| backend |")
			for _, wl := range v.workloads {
				fmt.Fprintf(b, " %s |", wl)
			}
			b.WriteString("\n|---|" + strings.Repeat("---:|", len(v.workloads)) + "\n")
			for _, be := range v.backends {
				fmt.Fprintf(b, "| %s |", be)
				for _, wl := range v.workloads {
					a := aggs[cell{v.name, wl, conc, be}]
					if a == nil || len(a.runs) == 0 {
						b.WriteString(" — |")
						continue
					}
					text := m.fmt(a.med(m.f))
					if be != v.reference {
						if ratio, _, ok := pairedRatio(a, aggs[cell{v.name, wl, conc, v.reference}], m.f); ok {
							text += fmt.Sprintf(" (%.2fx)", ratio)
						}
					}
					if a.noisy() {
						text += " †"
					}
					fmt.Fprintf(b, " %s |", text)
				}
				b.WriteString("\n")
			}
		}
	}
}

func writeWorkload(b *strings.Builder, v *labelView, wl string, aggs map[cell]*aggregate) {
	var cells []cell
	for _, conc := range v.concurrency {
		for _, be := range v.backends {
			c := cell{v.name, wl, conc, be}
			if aggs[c] != nil {
				cells = append(cells, c)
			}
		}
	}
	desc := ""
	if w, ok := LookupWorkload(wl); ok {
		desc = " — " + w.Description
	}
	fmt.Fprintf(b, "\n## %s%s\n", wl, desc)
	writeClientTable(b, v, cells, aggs)
	writeServerTable(b, v, cells, aggs)
	writeCommandTable(b, cells, aggs)
	writeProfileTable(b, cells, aggs)
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

// pairedRatio is the median, over repeats, of a's value divided by ref's in the
// same repeat, with the spread of those ratios. A cell's backends run back to
// back within a repeat, so pairing cancels host-load drift between repeats
// that a ratio of medians would keep.
func pairedRatio(a, ref *aggregate, f func(Result) float64) (ratio, spr float64, ok bool) {
	if ref == nil {
		return 0, 0, false
	}
	refByRepeat := map[int]float64{}
	for _, r := range ref.runs {
		refByRepeat[r.Repeat] = f(r)
	}
	var xs []float64
	for _, r := range a.runs {
		if d, ok := refByRepeat[r.Repeat]; ok && d != 0 {
			xs = append(xs, f(r)/d)
		}
	}
	if len(xs) == 0 {
		return 0, 0, false
	}
	return median(xs), spread(xs), true
}

func formatPairedRatio(a, ref *aggregate, f func(Result) float64) string {
	ratio, spr, ok := pairedRatio(a, ref, f)
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.2fx (±%.0f%%)", ratio, 50*spr)
}

func writeClientTable(b *strings.Builder, v *labelView, cells []cell, aggs map[cell]*aggregate) {
	b.WriteString("\n**Client** (median of measured runs)\n\n")
	fmt.Fprintf(b, "| conc | backend | runs | ops/s (±spread) | p50 µs | p99 µs | p99.9 µs | ops/s vs %s (paired) |\n", v.reference)
	b.WriteString("|---:|---|---:|---:|---:|---:|---:|---:|\n")
	for _, c := range cells {
		a := aggs[c]
		if len(a.runs) == 0 {
			continue
		}
		ref := aggs[cell{c.label, c.workload, c.concurrency, v.reference}]
		ops := func(r Result) float64 { return r.OpsPerSec }
		fmt.Fprintf(b, "| %d | %s | %d | %.0f (±%.0f%%) | %.1f | %.1f | %.1f | %s |\n",
			c.concurrency, c.backend, len(a.runs), a.med(ops), 50*spread(a.all(ops)),
			a.med(func(r Result) float64 { return r.Latency.P50 }),
			a.med(func(r Result) float64 { return r.Latency.P99 }),
			a.med(func(r Result) float64 { return r.Latency.P999 }),
			formatPairedRatio(a, ref, ops))
	}
}

func writeServerTable(b *strings.Builder, v *labelView, cells []cell, aggs map[cell]*aggregate) {
	b.WriteString("\n**Server cost per op** (median; event loop split into command execution and everything else; foreign cores is other processes' CPU, median and max over runs)\n\n")
	fmt.Fprintf(b, "| conc | backend | busy µs | vs %s (paired) | CPU µs | util (timeline min–max) | loop µs | cmd µs | other µs | ops/cycle | reads/op | writes/op | B in | B out | cmds/op | slowlog | errors | foreign cores (max) |\n", v.reference)
	b.WriteString("|---:|---|---:|---:|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, c := range cells {
		a := aggs[c]
		if len(a.runs) == 0 {
			continue
		}
		ref := aggs[cell{c.label, c.workload, c.concurrency, v.reference}]
		busy := func(r Result) float64 { return r.Server.BusyUsPerOp }
		foreign := func(r Result) float64 { return r.Interference.ForeignCores }
		var tlMin, tlMax []float64
		var slow, errs int64
		for _, r := range a.runs {
			tlMin = append(tlMin, r.Server.Timeline.UtilizationMin)
			tlMax = append(tlMax, r.Server.Timeline.UtilizationMax)
			slow += int64(r.Server.Slowlog.Entries)
			errs += r.Server.ErrorReplies
		}
		fmt.Fprintf(b, "| %d | %s | %.2f | %s | %.2f | %.2f (%.2f–%.2f) | %.2f | %.2f | %.2f | %.1f | %.3f | %.3f | %.0f | %.0f | %.2f | %d | %d | %.2f (%.2f) |\n",
			c.concurrency, c.backend, a.med(busy), formatPairedRatio(a, ref, busy),
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
			slow, errs, a.med(foreign), slices.Max(a.all(foreign)))
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

// profileRow renders one profile as table cells after the given leading cells.
func profileRow(leading string, p *ServerProfile) string {
	s := p.Summary
	return fmt.Sprintf("| %s | %.1f | %s | %s | %s | `%s` |",
		leading, s.BusyPercent, formatShares(s.Phases, 6), formatShares(s.Components, 7), formatShares(s.TopSelf, 4),
		relativeToRuns(p.Outputs.MainPprof))
}

func writeProfileTable(b *strings.Builder, cells []cell, aggs map[cell]*aggregate) {
	var rows []string
	for _, c := range cells {
		for _, r := range aggs[c].profiles {
			if r.ServerProfile != nil {
				rows = append(rows, profileRow(fmt.Sprintf("%d | %s", c.concurrency, c.backend), r.ServerProfile))
			}
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

// relativeToRuns shortens an artifact path to "<run>/<file>"; the report already
// groups by label.
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

func writeReclaim(b *strings.Builder, reclaims []ReclaimResult, opts ReportOptions) {
	var measured, profiled []ReclaimResult
	for _, r := range reclaims {
		if r.Profiled {
			profiled = append(profiled, r)
		} else {
			measured = append(measured, r)
		}
	}
	if len(reclaims) == 0 {
		return
	}
	b.WriteString("\n## reclaim — every field expires at one deadline; nothing reads them afterwards\n\n")
	b.WriteString("Reclaim work is main-thread busy time in the window net of harness commands and of the idle server's background rate (cron, timers) over the window. The B/field ratio compares used_memory growth with the same label's reference backend.\n\n")
	backends := map[string][]string{}
	for _, r := range measured {
		if !slices.Contains(backends[r.Label], r.Backend) {
			backends[r.Label] = append(backends[r.Label], r.Backend)
		}
	}
	refBytes := map[string]float64{}
	for _, r := range measured {
		if r.Backend == resolveReference(opts.Reference, backends[r.Label]) {
			refBytes[r.Label] = r.BytesPerField
		}
	}
	b.WriteString("| label | backend | fields | used_memory B/field | vs ref | dataset B/field | MEMORY USAGE B/field | load s | reclaimed | reclaim s | fields/s | idle busy % | work s | work µs/field | server expired_fields | module active/passive | sweeps | latency events | foreign cores |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|---:|---|---:|---:|---:|---:|---:|---:|---|---:|---|---:|\n")
	for _, r := range measured {
		reclaimed := "yes"
		if !r.Reclaimed {
			reclaimed = fmt.Sprintf("no (%d keys left)", r.RemainingKeys)
		}
		module := "—"
		if r.Server.ModuleActiveExpired+r.Server.ModulePassiveExpired > 0 {
			module = fmt.Sprintf("%d / %d", r.Server.ModuleActiveExpired, r.Server.ModulePassiveExpired)
		}
		vsRef := "—"
		if ref := refBytes[r.Label]; ref > 0 {
			vsRef = fmt.Sprintf("%.2fx", r.BytesPerField/ref)
		}
		var events []string
		for _, e := range r.Server.LatencyEvents {
			events = append(events, fmt.Sprintf("%s max %dms", e.Event, e.MaxMs))
		}
		fmt.Fprintf(b, "| %s | %s | %d | %.0f | %s | %.0f | %.0f | %.1f | %s | %.1f | %.0f | %.2f | %.3f | %.2f | %d | %s | %d | %s | %.2f |\n",
			r.Label, r.Backend, r.Keys*r.Fields, r.BytesPerField, vsRef, r.DatasetBytesPerField, r.MemoryUsagePerField,
			r.LoadSec, reclaimed, r.ReclaimSec, r.FieldsPerSec, 100*r.IdleBusyPerSec,
			r.Server.BusyMainSec-r.Server.HarnessSec-r.IdleBusyPerSec*r.Server.WindowSec, r.BusyUsPerField,
			r.Server.ExpiredFields, module, r.SweepCalls, strings.Join(events, ", "), r.Interference.ForeignCores)
	}
	var rows []string
	for _, r := range profiled {
		if r.ServerProfile != nil {
			rows = append(rows, profileRow(fmt.Sprintf("%s | %s", r.Label, r.Backend), r.ServerProfile))
		}
	}
	if len(rows) == 0 {
		return
	}
	b.WriteString("\n**Reclaim main-thread profile** (profiled pass from the opening of the reclaim window; % of busy samples)\n\n")
	b.WriteString("| label | backend | busy % | phases | components | top self | pprof |\n")
	b.WriteString("|---|---|---:|---|---|---|---|\n")
	b.WriteString(strings.Join(rows, "\n") + "\n")
}

func writeEnvironment(b *strings.Builder, loads []Result, reclaims []ReclaimResult) {
	type server struct {
		facts    Facts
		host     HostFacts
		backends []string
	}
	var servers []*server
	byKey := map[string]*server{}
	add := func(f Facts, h HostFacts, backend string) {
		key := fmt.Sprintf("%s|%d|%v", f.Executable, f.ProcessID, f.Modules)
		s := byKey[key]
		if s == nil {
			s = &server{facts: f, host: h}
			byKey[key] = s
			servers = append(servers, s)
		}
		if !slices.Contains(s.backends, backend) {
			s.backends = append(s.backends, backend)
		}
	}
	var windows []Interference
	for _, r := range loads {
		add(r.ServerFacts, r.Host, r.Backend)
		if !r.Profiled {
			windows = append(windows, r.Interference)
		}
	}
	for _, r := range reclaims {
		add(r.ServerFacts, r.Host, r.Backend)
		if !r.Profiled {
			windows = append(windows, r.Interference)
		}
	}
	if len(servers) == 0 {
		b.WriteString("No results.\n")
		return
	}
	b.WriteString("## environment\n\n")
	for _, s := range servers {
		f := s.facts
		rev := ""
		if f.TairHashRevision != "" {
			rev = " built from " + f.TairHashRevision
		}
		fmt.Fprintf(b, "- server pid %d (%s): valkey %s (%s, %s), modules [%s]%s; io-threads=%s hz=%s latency-tracking=%s (%s) slowlog>%sµs latency-monitor>%sms\n",
			f.ProcessID, strings.Join(s.backends, ", "), f.ValkeyVersion, f.Allocator, f.Multiplexing, strings.Join(f.Modules, ", "), rev,
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
	if h.Placement != "" {
		fmt.Fprintf(b, "- placement: %s\n", h.Placement)
	}
	var la []float64
	for _, r := range loads {
		la = append(la, r.LoadAvg[0], r.LoadAvg[1])
	}
	if len(la) > 0 {
		fmt.Fprintf(b, "- host 1-minute load average during runs: %.2f–%.2f\n", slices.Min(la), slices.Max(la))
	}
	writeInterference(b, windows)
}

// writeInterference summarises other processes' CPU over the measured windows
// and names the processes that used the most of it.
func writeInterference(b *strings.Builder, windows []Interference) {
	var cores []float64
	byName := map[string]float64{}
	for _, w := range windows {
		if w.WindowSec == 0 {
			continue // not captured
		}
		cores = append(cores, w.ForeignCores)
		for _, p := range w.Top {
			byName[p.Name] += p.CPUSec
		}
	}
	if len(cores) == 0 {
		return
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return byName[names[i]] > byName[names[j]] })
	var top []string
	for i, n := range names {
		if i == interferenceTop {
			break
		}
		top = append(top, fmt.Sprintf("%s %.1fs", n, byName[n]))
	}
	noisy := 0
	for _, c := range cores {
		if c > noisyForeignCores {
			noisy++
		}
	}
	fmt.Fprintf(b, "- other processes' CPU per measured window: median %.2f cores, max %.2f; %d of %d windows above %.0f core; largest consumers: %s\n",
		median(cores), slices.Max(cores), noisy, len(cores), noisyForeignCores, strings.Join(top, ", "))
}
