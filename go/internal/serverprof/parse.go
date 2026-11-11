package serverprof

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// sampleLine matches one call-graph node of a macOS `sample` report: tree
// drawing characters, the inclusive sample count, then the frame text.
var sampleLine = regexp.MustCompile(`^([ +!:|]*?)(\d+) (.*)$`)

// ParseSample reads a macOS `sample` report taken every period.
//
// The report's call graph is a tree of inclusive counts; a node's own samples
// are its count minus its children's. The same function appears once per call
// site, so stacks are keyed by function, not address.
func ParseSample(r io.Reader, period time.Duration) (*Profile, error) {
	type node struct {
		depth    int
		frame    Frame
		count    int64
		children int64
	}
	p := &Profile{Source: "macOS sample", Period: period}
	merged := map[string]int{}
	var path []node
	var thread string
	var main bool

	emit := func(n node) {
		self := n.count - n.children
		if self <= 0 {
			return
		}
		frames := make([]Frame, len(path)+1)
		for i, a := range path {
			frames[i] = a.frame
		}
		frames[len(path)] = n.frame
		key := thread + "\x00" + stackKey(frames)
		if i, ok := merged[key]; ok {
			p.Stacks[i].Count += self
			return
		}
		merged[key] = len(p.Stacks)
		p.Stacks = append(p.Stacks, Stack{Thread: thread, Main: main, Frames: frames, Count: self})
	}
	popTo := func(depth int) {
		for len(path) > 0 && path[len(path)-1].depth >= depth {
			n := path[len(path)-1]
			path = path[:len(path)-1]
			emit(n)
		}
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
	inGraph := false
	for sc.Scan() {
		line := sc.Text()
		if !inGraph {
			inGraph = strings.HasPrefix(line, "Call graph:")
			continue
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("unrecognised call-graph line %q", line)
		}
		count, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("count in %q: %w", line, err)
		}
		depth := (len(m[1]) - 4) / 2
		if depth == 0 {
			popTo(0)
			thread, main = parseSampleThread(m[3])
			continue
		}
		popTo(depth)
		if len(path) > 0 {
			path[len(path)-1].children += count
		}
		path = append(path, node{depth: depth, frame: parseSampleFrame(m[3]), count: count})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read sample report: %w", err)
	}
	if !inGraph {
		return nil, fmt.Errorf("no call graph in sample report")
	}
	popTo(0)
	return p, nil
}

// parseSampleThread reads "Thread_123   DispatchQueue_1: com.apple.main-thread  (serial)"
// or "Thread_124: bio_lazy_free".
func parseSampleThread(s string) (name string, main bool) {
	if strings.Contains(s, "com.apple.main-thread") {
		return "main", true
	}
	if _, after, ok := strings.Cut(s, ": "); ok {
		return strings.TrimSpace(after), false
	}
	return strings.TrimSpace(s), false
}

// parseSampleFrame reads "func  (in module) + 12  [0x1]".
func parseSampleFrame(s string) Frame {
	fn, rest, ok := strings.Cut(s, "  (in ")
	if !ok {
		return Frame{Func: strings.TrimSpace(s)}
	}
	mod, _, _ := strings.Cut(rest, ")")
	return Frame{Func: strings.TrimSpace(fn), Module: mod}
}

func stackKey(frames []Frame) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString(f.Func)
		b.WriteByte('\x01')
		b.WriteString(f.Module)
		b.WriteByte('\x02')
	}
	return b.String()
}

// ParsePerfScript reads `perf script` output for process pid sampled every
// period. Each event is a header line followed by one indented frame per line,
// leaf first, and a blank line. Samples whose thread id equals pid are the main
// thread.
func ParsePerfScript(r io.Reader, pid int, period time.Duration) (*Profile, error) {
	p := &Profile{Source: "linux perf", Period: period}
	merged := map[string]int{}
	var thread string
	var main bool
	var leafFirst []Frame
	inEvent := false

	flush := func() {
		if !inEvent {
			return
		}
		inEvent = false
		if len(leafFirst) == 0 {
			return
		}
		frames := make([]Frame, len(leafFirst))
		for i, f := range leafFirst {
			frames[len(leafFirst)-1-i] = f
		}
		leafFirst = leafFirst[:0]
		key := thread + "\x00" + stackKey(frames)
		if i, ok := merged[key]; ok {
			p.Stacks[i].Count++
			return
		}
		merged[key] = len(p.Stacks)
		p.Stacks = append(p.Stacks, Stack{Thread: thread, Main: main, Frames: frames, Count: 1})
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			flush()
			comm, tid, err := parsePerfHeader(line)
			if err != nil {
				return nil, err
			}
			thread, main = comm, tid == pid
			if main {
				thread = "main"
			}
			inEvent = true
			continue
		}
		if !inEvent {
			continue
		}
		leafFirst = append(leafFirst, parsePerfFrame(strings.TrimSpace(line)))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read perf script: %w", err)
	}
	flush()
	return p, nil
}

// parsePerfHeader reads "comm  pid/tid [cpu] time: period event:". comm may
// contain spaces, so the pid/tid field is located from the right.
func parsePerfHeader(line string) (comm string, tid int, err error) {
	fields := strings.Fields(line)
	for i := len(fields) - 1; i > 0; i-- {
		pidTid := fields[i]
		pidStr, tidStr, ok := strings.Cut(pidTid, "/")
		if !ok {
			continue
		}
		if _, perr := strconv.Atoi(pidStr); perr != nil {
			continue
		}
		if tid, err = strconv.Atoi(tidStr); err != nil {
			continue
		}
		return strings.Join(fields[:i], " "), tid, nil
	}
	return "", 0, fmt.Errorf("perf script header without pid/tid: %q", line)
}

// parsePerfFrame reads "addr symbol+0x1f (/path/to/module)".
func parsePerfFrame(s string) Frame {
	_, rest, ok := strings.Cut(s, " ")
	if !ok {
		return Frame{Func: s}
	}
	rest = strings.TrimSpace(rest)
	mod := ""
	if i := strings.LastIndex(rest, " ("); i >= 0 && strings.HasSuffix(rest, ")") {
		mod = rest[i+2 : len(rest)-1]
		if j := strings.LastIndexByte(mod, '/'); j >= 0 {
			mod = mod[j+1:]
		}
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "+0x"); i > 0 {
		rest = rest[:i]
	}
	return Frame{Func: rest, Module: mod}
}
