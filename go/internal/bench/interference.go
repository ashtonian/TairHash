package bench

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Interference is the CPU other processes on the host used during a window:
// everything except the benchmark client, its child processes (ps, the server
// profiler) and the server being measured. Other benchmark servers count, so
// idle module timers on them show up here.
type Interference struct {
	WindowSec float64 `json:"window_sec"`
	// ForeignCPUSec is the CPU seconds, user plus system, the other processes
	// consumed. Processes that both started and exited inside the window are
	// not seen, so it is a lower bound.
	ForeignCPUSec float64 `json:"foreign_cpu_sec"`
	// ForeignCores is ForeignCPUSec per second of window.
	ForeignCores float64 `json:"foreign_cores"`
	// Top lists the largest consumers, most first.
	Top []ProcessCPU `json:"top,omitempty"`
}

// ProcessCPU is one process's CPU use within a window.
type ProcessCPU struct {
	PID    int     `json:"pid"`
	Name   string  `json:"name"`
	CPUSec float64 `json:"cpu_sec"`
}

// interferenceTop is how many consumers an Interference keeps.
const interferenceTop = 5

type procStat struct {
	ppid int
	// cpu is cumulative user plus system seconds.
	cpu  float64
	name string
}

// cpuWatch brackets a window with two samples of every process's CPU time.
type cpuWatch struct {
	at      time.Time
	start   map[int]procStat
	exclude map[int]bool
}

// startCPUWatch samples the host's processes. exclude lists processes besides
// this one whose CPU is the measurement itself, such as the measured server.
func startCPUWatch(ctx context.Context, exclude ...int) (*cpuWatch, error) {
	start, err := sampleProcesses(ctx)
	if err != nil {
		return nil, err
	}
	w := &cpuWatch{at: time.Now(), start: start, exclude: map[int]bool{os.Getpid(): true}}
	for _, pid := range exclude {
		if pid > 0 {
			w.exclude[pid] = true
		}
	}
	return w, nil
}

// stop samples again and attributes the difference.
func (w *cpuWatch) stop(ctx context.Context) (Interference, error) {
	end, err := sampleProcesses(ctx)
	if err != nil {
		return Interference{}, err
	}
	return foreignCPU(w.start, end, w.exclude, os.Getpid(), time.Since(w.at)), nil
}

// foreignCPU attributes CPU used between two samples to processes other than
// the excluded ones and self's children. A process absent from start began
// inside the window, so all of its CPU time counts.
func foreignCPU(start, end map[int]procStat, exclude map[int]bool, self int, window time.Duration) Interference {
	in := Interference{WindowSec: window.Seconds()}
	var used []ProcessCPU
	for pid, e := range end {
		if exclude[pid] || e.ppid == self {
			continue
		}
		d := e.cpu
		if s, ok := start[pid]; ok && s.name == e.name {
			d -= s.cpu
		}
		if d <= 0 {
			continue
		}
		in.ForeignCPUSec += d
		used = append(used, ProcessCPU{PID: pid, Name: e.name, CPUSec: d})
	}
	sort.Slice(used, func(i, j int) bool { return used[i].CPUSec > used[j].CPUSec })
	if len(used) > interferenceTop {
		used = used[:interferenceTop]
	}
	in.Top = used
	if in.WindowSec > 0 {
		in.ForeignCores = in.ForeignCPUSec / in.WindowSec
	}
	return in
}

// sampleProcesses reads every process's cumulative CPU time: from procfs where
// it exists (Linux, clock-tick resolution), otherwise from ps (macOS reports
// centiseconds).
func sampleProcesses(ctx context.Context) (map[int]procStat, error) {
	if _, err := os.Stat("/proc/self/stat"); err == nil {
		return procfsProcesses()
	}
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,time=,comm=").Output()
	if err != nil {
		return nil, fmt.Errorf("sample process CPU with ps: %w", err)
	}
	return parsePS(out)
}

// parsePS parses `ps -A -o pid=,ppid=,time=,comm=`. comm is last because it
// may contain spaces.
func parsePS(out []byte) (map[int]procStat, error) {
	procs := map[int]procStat{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		cpu, err3 := parseCPUTime(f[2])
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, fmt.Errorf("ps line %q: unexpected format", sc.Text())
		}
		procs[pid] = procStat{ppid: ppid, cpu: cpu, name: filepath.Base(strings.Join(f[3:], " "))}
	}
	return procs, sc.Err()
}

// parseCPUTime parses ps's cumulative time, [[DD-]HH:]MM:SS[.frac]; macOS lets
// minutes exceed 59 instead of carrying into hours.
func parseCPUTime(s string) (float64, error) {
	days := 0.0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, fmt.Errorf("cpu time %q: %w", s, err)
		}
		days, s = float64(n), rest
	}
	var secs float64
	for _, part := range strings.Split(s, ":") {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, fmt.Errorf("cpu time %q: %w", s, err)
		}
		secs = secs*60 + v
	}
	return days*86400 + secs, nil
}

// clockTicks is USER_HZ, the unit of procfs CPU times. It is 100 on every
// mainstream Linux architecture; reading sysconf(_SC_CLK_TCK) would need cgo.
const clockTicks = 100

func procfsProcesses() (map[int]procStat, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("sample process CPU: %w", err)
	}
	procs := map[int]procStat{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue // exited since the directory was listed
		}
		if st, ok := parseProcStat(b); ok {
			procs[pid] = st
		}
	}
	return procs, nil
}

// parseProcStat parses /proc/PID/stat. The command sits in parentheses and may
// contain spaces and parentheses, so fields are counted from the last ')'.
func parseProcStat(b []byte) (procStat, bool) {
	s := string(b)
	open, closing := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || closing < open {
		return procStat{}, false
	}
	// After the command: state ppid pgrp session tty_nr tpgid flags minflt
	// cminflt majflt cmajflt utime stime ...
	f := strings.Fields(s[closing+1:])
	if len(f) < 13 {
		return procStat{}, false
	}
	ppid, err1 := strconv.Atoi(f[1])
	utime, err2 := strconv.ParseFloat(f[11], 64)
	stime, err3 := strconv.ParseFloat(f[12], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return procStat{}, false
	}
	return procStat{ppid: ppid, cpu: (utime + stime) / clockTicks, name: s[open+1 : closing]}, true
}
