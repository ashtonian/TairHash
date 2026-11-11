package bench

import (
	"bufio"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Server reads the server-side state a run is judged by.
type Server struct {
	vk valkey.Client
	db int

	moduleOnce sync.Once
	hasModule  bool
	moduleErr  error
}

// NewServer wraps vk, whose connections select db.
func NewServer(vk valkey.Client, db int) *Server { return &Server{vk: vk, db: db} }

// hasTairHash reports whether the TairHash module is loaded. It is checked once:
// probing with a module command on a server without it would add an error reply
// to the window being measured.
func (s *Server) hasTairHash(ctx context.Context) (bool, error) {
	s.moduleOnce.Do(func() {
		mods, err := s.moduleList(ctx)
		if err != nil {
			s.moduleErr = err
			return
		}
		for _, m := range mods {
			if m.name == "tairhash" {
				s.hasModule = true
			}
		}
	})
	return s.hasModule, s.moduleErr
}

type moduleInfo struct {
	name string
	args []string
}

func (s *Server) moduleList(ctx context.Context) ([]moduleInfo, error) {
	msgs, err := s.vk.Do(ctx, s.vk.B().ModuleList().Build()).ToArray()
	if err != nil {
		return nil, fmt.Errorf("MODULE LIST: %w", err)
	}
	out := make([]moduleInfo, 0, len(msgs))
	for _, m := range msgs {
		entry, err := m.AsMap()
		if err != nil {
			return nil, fmt.Errorf("MODULE LIST entry: %w", err)
		}
		nameMsg := entry["name"]
		var mi moduleInfo
		mi.name, _ = nameMsg.ToString()
		if a, ok := entry["args"]; ok {
			mi.args, _ = a.AsStrSlice()
		}
		out = append(out, mi)
	}
	return out, nil
}

func (s *Server) moduleExpireInfo(ctx context.Context) (string, error) {
	ok, err := s.hasTairHash(ctx)
	if err != nil || !ok {
		return "", err
	}
	text, err := s.vk.Do(ctx, s.vk.B().Arbitrary("EXHEXPIREINFO").Build()).ToString()
	if err != nil {
		return "", fmt.Errorf("EXHEXPIREINFO: %w", err)
	}
	return text, nil
}

// Snapshot is the server's full INFO (every section, including debug) at one instant.
type Snapshot struct {
	At   time.Time
	Info map[string]string
	// Raw is the INFO text as returned, kept for the run's artifacts.
	Raw string
	// ModuleExpire is the TairHash EXHEXPIREINFO text, empty without the module.
	ModuleExpire string
}

func (s Snapshot) float(key string) float64 {
	v, _ := strconv.ParseFloat(s.Info[key], 64)
	return v
}

func (s Snapshot) int(key string) int64 {
	v, _ := strconv.ParseInt(s.Info[key], 10, 64)
	return v
}

// Snapshot reads INFO everything, INFO debug (not part of "everything") and the
// module's expiry statistics when the module is loaded.
func (s *Server) Snapshot(ctx context.Context) (Snapshot, error) {
	at := time.Now()
	everything, err := s.vk.Do(ctx, s.vk.B().Info().Section("everything").Build()).ToString()
	if err != nil {
		return Snapshot{}, fmt.Errorf("INFO everything: %w", err)
	}
	debug, err := s.vk.Do(ctx, s.vk.B().Info().Section("debug").Build()).ToString()
	if err != nil {
		return Snapshot{}, fmt.Errorf("INFO debug: %w", err)
	}
	raw := everything + "\r\n" + debug
	snap := Snapshot{At: at, Info: parseInfoText(raw), Raw: raw}
	if snap.ModuleExpire, err = s.moduleExpireInfo(ctx); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// CmdStat is one INFO commandstats entry.
type CmdStat struct {
	Calls    int64 `json:"calls"`
	Usec     int64 `json:"usec"`
	Rejected int64 `json:"rejected_calls,omitempty"`
	Failed   int64 `json:"failed_calls,omitempty"`
}

func (s Snapshot) cmdStats() (map[string]CmdStat, error) {
	out := map[string]CmdStat{}
	for k, v := range s.Info {
		name, ok := strings.CutPrefix(k, "cmdstat_")
		if !ok {
			continue
		}
		st, err := parseCmdStat(v)
		if err != nil {
			return nil, fmt.Errorf("INFO %s: %w", k, err)
		}
		out[name] = st
	}
	return out, nil
}

// latencyPercentiles returns INFO latencystats: command -> "p50" -> µs.
func (s Snapshot) latencyPercentiles() map[string]map[string]float64 {
	out := map[string]map[string]float64{}
	for k, v := range s.Info {
		name, ok := strings.CutPrefix(k, "latency_percentiles_usec_")
		if !ok {
			continue
		}
		ps := map[string]float64{}
		for _, part := range strings.Split(v, ",") {
			p, val, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				ps[p] = f
			}
		}
		out[name] = ps
	}
	return out
}

func (s Snapshot) errorStats() map[string]int64 {
	out := map[string]int64{}
	for k, v := range s.Info {
		name, ok := strings.CutPrefix(k, "errorstat_")
		if !ok {
			continue
		}
		if n, ok := strings.CutPrefix(v, "count="); ok {
			out[name], _ = strconv.ParseInt(n, 10, 64)
		}
	}
	return out
}

// Facts describes the server a run measured, for the result record.
type Facts struct {
	ValkeyVersion  string            `json:"valkey_version"`
	ProcessID      int               `json:"process_id"`
	Multiplexing   string            `json:"multiplexing_api"`
	Allocator      string            `json:"mem_allocator"`
	Modules        []string          `json:"modules"`
	ModuleExpire   map[string]string `json:"tairhash_active_expire,omitempty"`
	Config         map[string]string `json:"config"`
	Executable     string            `json:"executable"`
	IOThreadsAlive string            `json:"io_threads_active"`
}

// factConfig lists the tunables recorded with every result because they change
// what a benchmark measures.
var factConfig = []string{
	"io-threads", "hz", "dynamic-hz", "active-expire-effort", "maxmemory", "maxmemory-policy",
	"appendonly", "save", "lazyfree-lazy-user-del", "lazyfree-lazy-expire", "busy-reply-threshold",
	"latency-tracking", "latency-tracking-info-percentiles", "latency-monitor-threshold",
	"slowlog-log-slower-than", "prefetch-batch-max-size", "hash-max-listpack-entries", "hash-max-listpack-value",
	"zset-max-listpack-entries", "zset-max-listpack-value",
}

// moduleExpireConfig are the TairHash active-expire settings (module load args).
var moduleExpireConfig = []string{
	"enable_active_expire", "tair_hash_active_expire_period", "tair_hash_active_expire_keys_per_loop",
	"tair_hash_active_expire_dbs_per_loop", "tair_hash_passive_expire_keys_per_loop",
}

// Facts reads server identity and the tunables that change results.
func (s *Server) Facts(ctx context.Context) (Facts, error) {
	info, err := s.vk.Do(ctx, s.vk.B().Info().Section("server", "memory", "stats").Build()).ToString()
	if err != nil {
		return Facts{}, fmt.Errorf("INFO server memory stats: %w", err)
	}
	kv := parseInfoText(info)
	f := Facts{
		ValkeyVersion:  kv["valkey_version"],
		Multiplexing:   kv["multiplexing_api"],
		Allocator:      kv["mem_allocator"],
		Executable:     kv["executable"],
		IOThreadsAlive: kv["io_threads_active"],
		Config:         map[string]string{},
	}
	f.ProcessID, _ = strconv.Atoi(kv["process_id"])

	mods, err := s.moduleList(ctx)
	if err != nil {
		return Facts{}, err
	}
	for _, m := range mods {
		f.Modules = append(f.Modules, strings.TrimSpace(m.name+" "+strings.Join(m.args, " ")))
	}
	for _, param := range factConfig {
		kv, err := s.vk.Do(ctx, s.vk.B().ConfigGet().Parameter(param).Build()).AsStrMap()
		if err != nil {
			return Facts{}, fmt.Errorf("CONFIG GET %s: %w", param, err)
		}
		if v, ok := kv[param]; ok {
			f.Config[param] = v
		}
	}
	raw, err := s.moduleExpireInfo(ctx)
	if err != nil {
		return Facts{}, err
	}
	if raw != "" {
		f.ModuleExpire = map[string]string{}
		info := parseInfoText(raw)
		for _, k := range moduleExpireConfig {
			f.ModuleExpire[k] = info[k]
		}
	}
	return f, nil
}

// ConfigDump returns CONFIG GET * as sorted "name value" lines.
func (s *Server) ConfigDump(ctx context.Context) (string, error) {
	kv, err := s.vk.Do(ctx, s.vk.B().ConfigGet().Parameter("*").Build()).AsStrMap()
	if err != nil {
		return "", fmt.Errorf("CONFIG GET *: %w", err)
	}
	lines := make([]string, 0, len(kv))
	for k, v := range kv {
		lines = append(lines, k+" "+v)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n", nil
}

// MemoryStats returns MEMORY STATS rendered as text.
func (s *Server) MemoryStats(ctx context.Context) (string, error) {
	v, err := s.vk.Do(ctx, s.vk.B().MemoryStats().Build()).ToAny()
	if err != nil {
		return "", fmt.Errorf("MEMORY STATS: %w", err)
	}
	return fmt.Sprintf("%v\n", v), nil
}

// MemoryUsage returns MEMORY USAGE key with exact sampling (SAMPLES 0).
func (s *Server) MemoryUsage(ctx context.Context, key string) (int64, error) {
	n, err := s.vk.Do(ctx, s.vk.B().MemoryUsage().Key(key).Samples(0).Build()).AsInt64()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("MEMORY USAGE %q: %w", key, err)
	}
	return n, nil
}

// ResetDiagnostics clears commandstats, latencystats, errorstats, the slowlog
// and latency-monitor history so the next window is measured in isolation.
func (s *Server) ResetDiagnostics(ctx context.Context) error {
	for _, cmd := range []valkey.Completed{
		s.vk.B().ConfigResetstat().Build(),
		s.vk.B().SlowlogReset().Build(),
		s.vk.B().LatencyReset().Build(),
	} {
		if err := s.vk.Do(ctx, cmd).Error(); err != nil {
			return fmt.Errorf("reset diagnostics: %w", err)
		}
	}
	return nil
}

// SlowEntry is one SLOWLOG entry.
type SlowEntry struct {
	ID         int64    `json:"id"`
	Unix       int64    `json:"unix"`
	DurationUs int64    `json:"duration_us"`
	Args       []string `json:"args"`
}

// Slowlog returns every SLOWLOG entry recorded since the last reset.
func (s *Server) Slowlog(ctx context.Context) ([]SlowEntry, error) {
	msgs, err := s.vk.Do(ctx, s.vk.B().SlowlogGet().Count(-1).Build()).ToArray()
	if err != nil {
		return nil, fmt.Errorf("SLOWLOG GET: %w", err)
	}
	out := make([]SlowEntry, 0, len(msgs))
	for _, m := range msgs {
		fields, err := m.ToArray()
		if err != nil || len(fields) < 4 {
			return nil, fmt.Errorf("SLOWLOG entry: unexpected shape (%v)", err)
		}
		var e SlowEntry
		e.ID, _ = fields[0].AsInt64()
		e.Unix, _ = fields[1].AsInt64()
		e.DurationUs, _ = fields[2].AsInt64()
		e.Args, _ = fields[3].AsStrSlice()
		out = append(out, e)
	}
	return out, nil
}

// LatencyEvent is one LATENCY LATEST row: the worst and latest spike of an
// event class above latency-monitor-threshold.
type LatencyEvent struct {
	Event    string `json:"event"`
	Unix     int64  `json:"unix"`
	LatestMs int64  `json:"latest_ms"`
	MaxMs    int64  `json:"max_ms"`
}

// LatencyLatest returns LATENCY LATEST.
func (s *Server) LatencyLatest(ctx context.Context) ([]LatencyEvent, error) {
	msgs, err := s.vk.Do(ctx, s.vk.B().LatencyLatest().Build()).ToArray()
	if err != nil {
		return nil, fmt.Errorf("LATENCY LATEST: %w", err)
	}
	out := make([]LatencyEvent, 0, len(msgs))
	for _, m := range msgs {
		fields, err := m.ToArray()
		if err != nil || len(fields) < 4 {
			return nil, fmt.Errorf("LATENCY LATEST row: unexpected shape (%v)", err)
		}
		var e LatencyEvent
		e.Event, _ = fields[0].ToString()
		e.Unix, _ = fields[1].AsInt64()
		e.LatestMs, _ = fields[2].AsInt64()
		e.MaxMs, _ = fields[3].AsInt64()
		out = append(out, e)
	}
	return out, nil
}

// LatencyHistory returns LATENCY HISTORY event as [unix, ms] pairs.
func (s *Server) LatencyHistory(ctx context.Context, event string) ([][2]int64, error) {
	msgs, err := s.vk.Do(ctx, s.vk.B().LatencyHistory().Event(event).Build()).ToArray()
	if err != nil {
		return nil, fmt.Errorf("LATENCY HISTORY %s: %w", event, err)
	}
	out := make([][2]int64, 0, len(msgs))
	for _, m := range msgs {
		pair, err := m.AsIntSlice()
		if err != nil || len(pair) != 2 {
			return nil, fmt.Errorf("LATENCY HISTORY %s row: unexpected shape (%v)", event, err)
		}
		out = append(out, [2]int64{pair[0], pair[1]})
	}
	return out, nil
}

// Flush empties the selected database.
func (s *Server) Flush(ctx context.Context) error {
	if err := s.vk.Do(ctx, s.vk.B().Flushdb().Sync().Build()).Error(); err != nil {
		return fmt.Errorf("FLUSHDB: %w", err)
	}
	return nil
}

// Keys returns the number of keys in the selected database.
func (s *Server) Keys(ctx context.Context) (int64, error) {
	n, err := s.vk.Do(ctx, s.vk.B().Dbsize().Build()).ToInt64()
	if err != nil {
		return 0, fmt.Errorf("DBSIZE: %w", err)
	}
	return n, nil
}

// quickStats reads the INFO sections the timeline samples: cheap enough to call
// every second during a run.
func (s *Server) quickStats(ctx context.Context) (map[string]string, error) {
	raw, err := s.vk.Do(ctx, s.vk.B().Info().Section("cpu", "stats", "memory", "clients").Build()).ToString()
	if err != nil {
		return nil, fmt.Errorf("INFO cpu stats memory clients: %w", err)
	}
	return parseInfoText(raw), nil
}

func parseInfoText(raw string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[k] = v
		}
	}
	return out
}

func parseCmdStat(v string) (CmdStat, error) {
	var st CmdStat
	for _, part := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		var err error
		switch k {
		case "calls":
			st.Calls, err = strconv.ParseInt(val, 10, 64)
		case "usec":
			st.Usec, err = strconv.ParseInt(val, 10, 64)
		case "rejected_calls":
			st.Rejected, err = strconv.ParseInt(val, 10, 64)
		case "failed_calls":
			st.Failed, err = strconv.ParseInt(val, 10, 64)
		}
		if err != nil {
			return CmdStat{}, fmt.Errorf("%s=%q: %w", k, val, err)
		}
	}
	return st, nil
}

// moduleExpireCounts parses EXHEXPIREINFO's per-db line for db:
// "db: 9, active_expired_fields: 12, passive_expired_fields: 3".
func moduleExpireCounts(text string, db int) (active, passive int64, ok bool) {
	prefix := "db: " + strconv.Itoa(db) + ","
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		for _, part := range strings.Split(line, ",") {
			k, v, found := strings.Cut(strings.TrimSpace(part), ": ")
			if !found {
				continue
			}
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			switch k {
			case "active_expired_fields":
				active = n
			case "passive_expired_fields":
				passive = n
			}
		}
		return active, passive, true
	}
	return 0, 0, false
}
