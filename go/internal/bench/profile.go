package bench

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ashtonian/TairHash/go/internal/serverprof"
)

// Profiler samples the server process for a measured window by running an
// external command, then converts the capture with serverprof. The template's
// {pid}, {secs} and {out} placeholders are substituted; each remaining
// whitespace-separated word is one argument.
type Profiler struct {
	Format   serverprof.Format
	Template string
	Period   time.Duration
}

// NewProfiler builds a profiler of kind auto, sample, perf or none. A non-empty
// template overrides the command for the chosen kind.
func NewProfiler(kind, template string) (*Profiler, error) {
	if kind == "auto" {
		switch {
		case runtime.GOOS == "darwin":
			kind = "sample"
		case runtime.GOOS == "linux":
			if _, err := exec.LookPath("perf"); err == nil {
				kind = "perf"
			} else {
				kind = "none"
			}
		default:
			kind = "none"
		}
	}
	var p *Profiler
	switch kind {
	case "none":
		return nil, nil
	case "sample":
		// 1ms interval; -mayDie keeps sample from failing if the server exits.
		p = &Profiler{Format: serverprof.FormatSample, Template: "sample {pid} {secs} 1 -mayDie -file {out}", Period: time.Millisecond}
	case "perf":
		p = &Profiler{Format: serverprof.FormatPerf, Template: "perf record -F 999 -g -p {pid} -o {out} -- sleep {secs}", Period: time.Second / 999}
	default:
		return nil, fmt.Errorf("unknown server profiler %q (want auto, sample, perf or none)", kind)
	}
	if template != "" {
		p.Template = template
	}
	return p, nil
}

// rawName is the capture's file name inside the run's artifacts.
func (p *Profiler) rawName() string {
	if p.Format == serverprof.FormatPerf {
		return "server.perf.data"
	}
	return "server.sample.txt"
}

// Start launches the profiler and returns a function that waits for it.
func (p *Profiler) Start(ctx context.Context, pid int, d time.Duration, out string) (func() error, error) {
	secs := int(d.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	r := strings.NewReplacer("{pid}", strconv.Itoa(pid), "{secs}", strconv.Itoa(secs), "{out}", out)
	argv := strings.Fields(r.Replace(p.Template))
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty profiler template")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start profiler %q: %w", argv[0], err)
	}
	return func() error {
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}, nil
}

// loadAverage returns the host's one-minute load average, so noisy runs on a
// shared machine can be spotted in the results.
func loadAverage() (float64, error) {
	var raw string
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		raw = string(b)
	} else {
		out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			return 0, fmt.Errorf("read load average: %w", err)
		}
		raw = strings.Trim(strings.TrimSpace(string(out)), "{} ")
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return 0, fmt.Errorf("unparseable load average %q", raw)
	}
	return strconv.ParseFloat(fields[0], 64)
}
