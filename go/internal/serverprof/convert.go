package serverprof

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// Format names a raw capture format.
type Format string

const (
	// FormatSample is a macOS `sample` text report.
	FormatSample Format = "sample"
	// FormatPerf is a Linux perf.data file, decoded with `perf script`.
	FormatPerf Format = "perf"
)

// Outputs are the files Convert writes next to the raw capture.
type Outputs struct {
	MainPprof string `json:"main_pprof"`
	AllPprof  string `json:"all_pprof"`
	Folded    string `json:"folded"`
	Summary   string `json:"summary"`
}

// Convert parses a raw capture of process pid and writes, under prefix:
// .main.pprof (main thread), .all.pprof (every thread), .folded and
// .summary.json. It returns the main-thread summary.
func Convert(ctx context.Context, format Format, raw string, pid int, period time.Duration, prefix string, top int) (Summary, Outputs, error) {
	p, err := load(ctx, format, raw, pid, period)
	if err != nil {
		return Summary{}, Outputs{}, err
	}
	out := Outputs{
		MainPprof: prefix + ".main.pprof",
		AllPprof:  prefix + ".all.pprof",
		Folded:    prefix + ".folded",
		Summary:   prefix + ".summary.json",
	}
	if err := writeFile(out.MainPprof, func(w io.Writer) error { return p.WritePprof(w, MainThread) }); err != nil {
		return Summary{}, Outputs{}, err
	}
	if err := writeFile(out.AllPprof, func(w io.Writer) error { return p.WritePprof(w, nil) }); err != nil {
		return Summary{}, Outputs{}, err
	}
	if err := writeFile(out.Folded, p.WriteFolded); err != nil {
		return Summary{}, Outputs{}, err
	}
	sum := Summarize(p, top)
	if err := writeFile(out.Summary, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(sum)
	}); err != nil {
		return Summary{}, Outputs{}, err
	}
	return sum, out, nil
}

func load(ctx context.Context, format Format, raw string, pid int, period time.Duration) (*Profile, error) {
	switch format {
	case FormatSample:
		f, err := os.Open(raw)
		if err != nil {
			return nil, fmt.Errorf("open sample report: %w", err)
		}
		defer f.Close()
		p, err := ParseSample(f, period)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", raw, err)
		}
		return p, nil
	case FormatPerf:
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "perf", "script", "-i", raw, "-F", "comm,pid,tid,time,period,event,ip,sym,dso")
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("perf script -i %s: %w: %s", raw, err, bytes.TrimSpace(stderr.Bytes()))
		}
		p, err := ParsePerfScript(&stdout, pid, period)
		if err != nil {
			return nil, fmt.Errorf("parse perf script of %s: %w", raw, err)
		}
		return p, nil
	}
	return nil, fmt.Errorf("unknown profile format %q", format)
}

func writeFile(path string, fn func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := fn(f); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Close()
}
