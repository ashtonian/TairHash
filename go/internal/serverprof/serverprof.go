// Package serverprof turns stack samples of a running valkey-server into pprof
// profiles, folded stacks and a phase breakdown of the main thread.
//
// Two sources are supported: macOS `sample` reports (wall-clock sampling of
// every thread, idle included) and Linux `perf script` output (on-CPU sampling).
// Both are reduced to the same Profile so the analysis is source-independent.
package serverprof

import (
	"compress/gzip"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/google/pprof/profile"
)

// Frame is one function in a stack.
type Frame struct {
	Func   string
	Module string
}

// Stack is a distinct call path and how many samples ended exactly at its leaf.
type Stack struct {
	Thread string
	// Main marks the server's main thread, which runs every command.
	Main bool
	// Frames run from the outermost caller to the sampled leaf.
	Frames []Frame
	Count  int64
}

// Profile is a set of stack samples taken at a fixed period.
type Profile struct {
	Source string
	Period time.Duration
	Stacks []Stack
}

// WritePprof writes a gzipped pprof profile of the stacks keep accepts, with
// sample counts and wall/CPU time derived from the sampling period.
func (p *Profile) WritePprof(w io.Writer, keep func(Stack) bool) error {
	out := &profile.Profile{
		SampleType: []*profile.ValueType{
			{Type: "samples", Unit: "count"},
			{Type: "time", Unit: "nanoseconds"},
		},
		PeriodType: &profile.ValueType{Type: "time", Unit: "nanoseconds"},
		Period:     p.Period.Nanoseconds(),
		Comments:   []string{"source: " + p.Source},
	}
	mappings := map[string]*profile.Mapping{}
	mapping := func(module string) *profile.Mapping {
		if m, ok := mappings[module]; ok {
			return m
		}
		m := &profile.Mapping{ID: uint64(len(mappings) + 1), File: module, HasFunctions: true}
		mappings[module] = m
		out.Mapping = append(out.Mapping, m)
		return m
	}
	// pprof treats the first mapping as the main binary.
	if main := p.mainModule(); main != "" {
		mapping(main)
	}
	functions := map[Frame]*profile.Function{}
	locations := map[Frame]*profile.Location{}
	location := func(f Frame) *profile.Location {
		if loc, ok := locations[f]; ok {
			return loc
		}
		fn := &profile.Function{ID: uint64(len(functions) + 1), Name: f.Func, SystemName: f.Func, Filename: f.Module}
		functions[f] = fn
		out.Function = append(out.Function, fn)
		loc := &profile.Location{ID: uint64(len(locations) + 1), Mapping: mapping(f.Module), Line: []profile.Line{{Function: fn}}}
		locations[f] = loc
		out.Location = append(out.Location, loc)
		return loc
	}
	for _, s := range p.Stacks {
		if keep != nil && !keep(s) {
			continue
		}
		locs := make([]*profile.Location, 0, len(s.Frames))
		for i := len(s.Frames) - 1; i >= 0; i-- {
			locs = append(locs, location(s.Frames[i]))
		}
		out.Sample = append(out.Sample, &profile.Sample{
			Location: locs,
			Value:    []int64{s.Count, s.Count * p.Period.Nanoseconds()},
			Label:    map[string][]string{"thread": {s.Thread}},
		})
	}
	if err := out.CheckValid(); err != nil {
		return fmt.Errorf("build pprof profile: %w", err)
	}
	gz := gzip.NewWriter(w)
	if err := out.WriteUncompressed(gz); err != nil {
		return fmt.Errorf("write pprof profile: %w", err)
	}
	return gz.Close()
}

// WriteFolded writes one "thread;frame;frame count" line per stack, the input
// format of flame-graph tools.
func (p *Profile) WriteFolded(w io.Writer) error {
	lines := make([]string, 0, len(p.Stacks))
	for _, s := range p.Stacks {
		var b strings.Builder
		b.WriteString(strings.ReplaceAll(s.Thread, ";", ":"))
		for _, f := range s.Frames {
			b.WriteByte(';')
			b.WriteString(strings.ReplaceAll(f.Func, ";", ":"))
		}
		fmt.Fprintf(&b, " %d", s.Count)
		lines = append(lines, b.String())
	}
	sort.Strings(lines)
	for _, l := range lines {
		if _, err := io.WriteString(w, l+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// mainModule is the server binary's module name, found from the main thread's
// outermost frames.
func (p *Profile) mainModule() string {
	for _, st := range p.Stacks {
		for _, f := range st.Frames {
			if strings.Contains(f.Module, "valkey-server") || strings.Contains(f.Module, "redis-server") {
				return f.Module
			}
		}
	}
	return ""
}

// MainThread selects the main thread's stacks.
func MainThread(s Stack) bool { return s.Main }
