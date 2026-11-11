package serverprof

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A trimmed macOS sample report: the main thread is idle in kevent for 6
// samples and runs a command for 4 (3 in HSET's hash code, 1 in malloc), plus
// one idle background thread.
const sampleReport = `Analysis of sampling valkey-server (pid 1) every 1 millisecond
Process:         valkey-server [1]

Call graph:
    10 Thread_1   DispatchQueue_1: com.apple.main-thread  (serial)
    + 10 start  (in dyld) + 7184  [0x1]
    +   10 main  (in valkey-server) + 3372  [0x2]
    +     6 aeProcessEvents  (in valkey-server) + 100  [0x3]
    +     ! 6 aeApiPoll  (in valkey-server) + 100  [0x4]
    +     !   6 kevent  (in libsystem_kernel.dylib) + 8  [0x5]
    +     4 aeProcessEvents  (in valkey-server) + 364  [0x6]
    +       4 readQueryFromClient  (in valkey-server) + 176  [0x7]
    +         4 call  (in valkey-server) + 780  [0x8]
    +           3 hsetCommand  (in valkey-server) + 12  [0x9]
    +           ! 2 hashTypeSet  (in valkey-server) + 0,60  [0xa,0xb]
    +           1 DYLD-STUB$$malloc  (in valkey-server) + 0  [0xc]
    10 Thread_2: bio_lazy_free
    + 10 thread_start  (in libsystem_pthread.dylib) + 8  [0xd]
    +   10 __psynch_cvwait  (in libsystem_kernel.dylib) + 8  [0xe]

Total number in stack (recursive counted multiple, when >=5):
`

func TestParseSample(t *testing.T) {
	p, err := ParseSample(strings.NewReader(sampleReport), time.Millisecond)
	require.NoError(t, err)

	var main, other int64
	for _, s := range p.Stacks {
		if s.Main {
			main += s.Count
		} else {
			other += s.Count
		}
	}
	assert.Equal(t, int64(10), main, "self counts of the main thread sum to its total")
	assert.Equal(t, int64(10), other)

	sum := Summarize(p, 5)
	assert.Equal(t, int64(10), sum.Samples)
	assert.Equal(t, int64(6), sum.Idle)
	assert.Equal(t, int64(4), sum.Busy)
	assert.InDelta(t, 40.0, sum.BusyPercent, 1e-9)
	assert.Equal(t, []Share{{Name: "command", Samples: 4, Percent: 100}}, sum.Phases)
	assert.Equal(t, []Share{
		{Name: "hash", Samples: 3, Percent: 75},
		{Name: "alloc", Samples: 1, Percent: 25},
	}, sum.Components, "hsetCommand's own sample and hashTypeSet both count as hash; the stub resolves to malloc")
	assert.Empty(t, sum.Unclassified)
}

func TestWritePprof(t *testing.T) {
	p, err := ParseSample(strings.NewReader(sampleReport), time.Millisecond)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, p.WritePprof(&buf, MainThread))
	prof, err := profile.Parse(&buf)
	require.NoError(t, err)
	assert.Equal(t, "valkey-server", prof.Mapping[0].File, "server binary is the main mapping")

	var samples, nanos int64
	for _, s := range prof.Sample {
		samples += s.Value[0]
		nanos += s.Value[1]
		assert.Equal(t, []string{"main"}, s.Label["thread"])
	}
	assert.Equal(t, int64(10), samples)
	assert.Equal(t, int64(10*time.Millisecond), nanos)
}

func TestWriteFolded(t *testing.T) {
	p, err := ParseSample(strings.NewReader(sampleReport), time.Millisecond)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, p.WriteFolded(&buf))
	assert.Contains(t, buf.String(), "main;start;main;aeProcessEvents;readQueryFromClient;call;hsetCommand;hashTypeSet 2\n")
	assert.Contains(t, buf.String(), "bio_lazy_free;thread_start;__psynch_cvwait 10\n")
}

const perfScript = `valkey-server 4242/4242 1000.000001:    1001001 cpu-clock:
	    55d5c1a2b3c4 hashTypeSet+0x24 (/usr/local/bin/valkey-server)
	    55d5c1a2b000 call+0x10 (/usr/local/bin/valkey-server)
	    55d5c1a2a000 main+0x99 (/usr/local/bin/valkey-server)

valkey-server 4242/4242 1000.001002:    1001001 cpu-clock:
	    55d5c1a2b3c4 hashTypeSet+0x24 (/usr/local/bin/valkey-server)
	    55d5c1a2b000 call+0x10 (/usr/local/bin/valkey-server)
	    55d5c1a2a000 main+0x99 (/usr/local/bin/valkey-server)

bio_lazy_free 4242/4250 1000.002003:    1001001 cpu-clock:
	ffffffff81000000 [unknown] ([kernel.kallsyms])

`

func TestParsePerfScript(t *testing.T) {
	p, err := ParsePerfScript(strings.NewReader(perfScript), 4242, time.Millisecond)
	require.NoError(t, err)
	require.Len(t, p.Stacks, 2)

	main := p.Stacks[0]
	assert.True(t, main.Main)
	assert.Equal(t, int64(2), main.Count, "identical stacks merge")
	assert.Equal(t, []Frame{
		{Func: "main", Module: "valkey-server"},
		{Func: "call", Module: "valkey-server"},
		{Func: "hashTypeSet", Module: "valkey-server"},
	}, main.Frames, "frames are reordered root first")

	bio := p.Stacks[1]
	assert.False(t, bio.Main)
	assert.Equal(t, "bio_lazy_free", bio.Thread)
	assert.Equal(t, []Frame{{Func: "[unknown]", Module: "[kernel.kallsyms]"}}, bio.Frames)

	sum := Summarize(p, 3)
	assert.Equal(t, []Share{{Name: "command", Samples: 2, Percent: 100}}, sum.Phases)
}

func TestTimerPhaseOutranksCommand(t *testing.T) {
	frames := []Frame{{Func: "aeProcessEvents"}, {Func: "processTimeEvents"}, {Func: "moduleTimerHandler"}, {Func: "call"}, {Func: "scanCommand"}}
	assert.Equal(t, "timers", classifyPhase(frames), "module timers run commands via RedisModule_Call")
}
