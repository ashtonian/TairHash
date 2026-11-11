package serverprof

import (
	"sort"
	"strings"
)

// Phases of the main thread's event loop, in attribution priority: a stack is
// assigned the first phase whose marker frame appears anywhere in it. Timers
// outrank command because module timers execute commands through
// RedisModule_Call, and that work belongs to the timer, not to a client.
var phaseRules = []struct {
	phase   string
	markers []string
}{
	{"timers", []string{"processTimeEvents"}},
	{"command", []string{"call"}},
	{"write", []string{"writeToClient", "_writeToClient", "writevToClient", "handleClientsWithPendingWrites", "handleClientsWithPendingWritesUsingThreads"}},
	{"expire", []string{"activeExpireCycle", "activeExpireCycleTryExpire"}},
	{"read+parse", []string{"readQueryFromClient", "processInputBuffer"}},
	{"poll", []string{"aeApiPoll"}},
	{"beforeSleep", []string{"beforeSleep", "afterSleep"}},
}

// Components classify where busy time goes inside a phase, by the leaf-most
// frame that matches. Rules are checked per frame from leaf to root, so a
// malloc called from Lua counts as alloc and a hash lookup Lua issued counts as
// hash, not as Lua.
var componentRules = []struct {
	component string
	match     func(Frame) bool
}{
	{"alloc", anyOf(
		exact("malloc", "free", "realloc", "calloc", "malloc_size", "malloc_usable_size", "_free", "_malloc_zone_malloc", "_malloc_zone_calloc", "_malloc_zone_realloc"),
		prefixes("zmalloc", "zfree", "zrealloc", "zcalloc", "ztrymalloc", "valkey_malloc", "xzm_", "_xzm_", "nanov2_", "szone_", "malloc_zone_", "je_"),
	)},
	{"memcpy", prefixes("_platform_mem", "_platform_bzero", "memcpy", "memmove", "memset", "memcmp", "bzero", "__memcpy", "__memmove", "__memset", "__memcmp")},
	{"syscall", exact("read", "write", "writev", "kevent", "epoll_wait", "epoll_ctl", "__psynch_cvwait", "__select", "sendmsg", "recvfrom")},
	{"module", func(f Frame) bool { return strings.Contains(f.Module, "tairhash") }},
	{"module-api", prefixes("VM_", "RM_", "module", "moduleCall")},
	{"lua", prefixes("lua", "luaV_", "luaD_", "luaH_", "luaS_", "luaC_", "luaF_", "luaG_", "luaO_", "luaM_", "luaK_", "luaT_", "luaX_", "luaY_", "luaZ_", "luaL_", "luaB_", "lua_", "script", "fcall", "evalGeneric", "functions", "struct_", "str_", "math_")},
	{"hash", prefixes("hashType", "hashtable", "hashTypeGet", "lp", "listpack", "hset", "hget", "hdel", "hgetall", "hincr", "hsetex", "hpexpire", "hpttl", "volatileSet", "vset", "entry")},
	{"zset", prefixes("zsl", "zset", "zadd", "zrem", "zrange", "genericZrange", "zzl")},
	{"keyspace", prefixes("lookupKey", "dbFind", "dbAdd", "dbGeneric", "dbDelete", "dbSync", "kvstore", "getExpire", "expireIfNeeded", "keyIsExpired", "setExpire", "removeExpire", "signalModifiedKey", "notifyKeyspaceEvent", "deleteExpiredKeyAndPropagate", "objectGetExpire")},
	{"reply", prefixes("addReply", "_addReply", "setDeferred", "prepareClientToWrite", "addReplyDeferredLen", "addReplyProto")},
	{"string/object", prefixes("sds", "_sds", "createObject", "createStringObject", "createRawStringObject", "createEmbeddedStringObject", "decrRefCount", "incrRefCount", "tryObjectEncoding", "getDecodedObject", "string2ll", "ll2string", "strlen")},
	{"parse", prefixes("processMultibulkBuffer", "processInlineBuffer", "parseMultibulk", "readToQueryBuf", "processInputBuffer")},
	{"dispatch", prefixes("processCommand", "call", "lookupCommand", "commandProcessed", "resetClient", "freeClientArgv", "updateCommandLatencyHistogram", "updateStatsOnCommand", "propagate", "alsoPropagate", "postExecutionUnitOperations", "afterCommand", "prepareCommand", "getKeysUsing", "getKeysFromCommand", "ACLCheck", "dictSdsCaseHash", "siphash", "strcasecmp", "dictFind")},
	{"prefetch", prefixes("prefetchCommandQueueKeys", "addKeysToIncrFindBatch", "prefetch", "hashtableIncrementalFind")},
	{"write-path", prefixes("postWriteToClient", "clientHasPendingReplies", "_postWriteToClient", "writeToClient", "_writeToClient")},
	// localtime_r re-reads TZ under a libc lock; Valkey refreshes its cached
	// time (including daylight-saving state) around every event-loop iteration.
	{"clock", prefixes("localtime", "_st_localsub", "timesub", "__findenv_locked", "getenv", "_NSGetEnviron", "tzset", "notify_check", "getMonotonicUs", "ustime", "mstime", "updateCachedTime", "gettimeofday", "clock_gettime", "mach_absolute_time")},
	{"eventloop", prefixes("aeProcessEvents", "aeApiPoll", "aeMain", "beforeSleep", "afterSleep", "IOThreads", "processIOThreads", "trySendReadToIOThreads", "trySendWriteToIOThreads", "connSocketEventHandler", "handleClientsBlockedOn", "processEventsWhileBlocked", "whileBlockedCron", "sigprocmask", "_tlv_get_addr")},
}

// stubPrefix marks dynamic-linker trampolines; the target function follows it.
const stubPrefix = "DYLD-STUB$$"

func prefixes(ps ...string) func(Frame) bool {
	return func(f Frame) bool {
		for _, p := range ps {
			if strings.HasPrefix(f.Func, p) {
				return true
			}
		}
		return false
	}
}

func anyOf(rules ...func(Frame) bool) func(Frame) bool {
	return func(f Frame) bool {
		for _, r := range rules {
			if r(f) {
				return true
			}
		}
		return false
	}
}

func exact(names ...string) func(Frame) bool {
	return func(f Frame) bool {
		for _, n := range names {
			if f.Func == n {
				return true
			}
		}
		return false
	}
}

// Share is a fraction of the main thread's busy samples.
type Share struct {
	Name    string  `json:"name"`
	Samples int64   `json:"samples"`
	Percent float64 `json:"percent"`
}

// Summary describes how the main thread spent the profiled window.
type Summary struct {
	Source string `json:"source"`
	// Samples counts every main-thread sample; Idle those blocked waiting for
	// events (macOS samples idle threads, perf does not); Busy the rest.
	Samples int64 `json:"samples"`
	Idle    int64 `json:"idle"`
	Busy    int64 `json:"busy"`
	// BusyPercent is Busy over Samples: the profiler's view of utilisation.
	BusyPercent float64 `json:"busy_percent"`
	// Phases and Components are percentages of Busy.
	Phases     []Share `json:"phases"`
	Components []Share `json:"components"`
	// PhaseComponents splits each phase by component.
	PhaseComponents map[string][]Share `json:"phase_components"`
	// TopSelf ranks functions by samples where they were the leaf; TopCum by
	// samples where they were anywhere on the stack.
	TopSelf []Share `json:"top_self"`
	TopCum  []Share `json:"top_cum"`
	// Unclassified ranks the leaf functions that fell into the "other"
	// component, so gaps in the classification rules stay visible.
	Unclassified []Share `json:"unclassified"`
}

// idleLeaves are the blocking calls the event loop waits in.
var idleLeaves = map[string]bool{"kevent": true, "epoll_wait": true, "epoll_pwait": true, "__select": true}

// Summarize classifies the main thread's samples. top bounds TopSelf/TopCum.
func Summarize(p *Profile, top int) Summary {
	s := Summary{Source: p.Source, PhaseComponents: map[string][]Share{}}
	phases := map[string]int64{}
	components := map[string]int64{}
	phaseComponents := map[string]map[string]int64{}
	self := map[string]int64{}
	cum := map[string]int64{}
	unclassified := map[string]int64{}

	for _, st := range p.Stacks {
		if !st.Main || len(st.Frames) == 0 {
			continue
		}
		s.Samples += st.Count
		leaf := st.Frames[len(st.Frames)-1]
		phase := classifyPhase(st.Frames)
		if phase == "poll" && idleLeaves[leaf.Func] {
			s.Idle += st.Count
			continue
		}
		s.Busy += st.Count
		comp := classifyComponent(st.Frames)
		phases[phase] += st.Count
		components[comp] += st.Count
		if phaseComponents[phase] == nil {
			phaseComponents[phase] = map[string]int64{}
		}
		phaseComponents[phase][comp] += st.Count
		self[label(leaf)] += st.Count
		if comp == "other" {
			unclassified[label(leaf)] += st.Count
		}
		seen := map[string]bool{}
		for _, f := range st.Frames {
			if l := label(f); !seen[l] {
				seen[l] = true
				cum[l] += st.Count
			}
		}
	}
	if s.Samples > 0 {
		s.BusyPercent = 100 * float64(s.Busy) / float64(s.Samples)
	}
	s.Phases = shares(phases, s.Busy, 0)
	s.Components = shares(components, s.Busy, 0)
	for ph, m := range phaseComponents {
		s.PhaseComponents[ph] = shares(m, s.Busy, 0)
	}
	s.TopSelf = shares(self, s.Busy, top)
	s.TopCum = shares(cum, s.Busy, top)
	s.Unclassified = shares(unclassified, s.Busy, top)
	return s
}

func classifyPhase(frames []Frame) string {
	for _, rule := range phaseRules {
		for _, f := range frames {
			for _, m := range rule.markers {
				if f.Func == m {
					return rule.phase
				}
			}
		}
	}
	return "other"
}

func classifyComponent(frames []Frame) string {
	for i := len(frames) - 1; i >= 0; i-- {
		f := frames[i]
		f.Func = strings.TrimPrefix(f.Func, stubPrefix)
		for _, rule := range componentRules {
			if rule.match(f) {
				return rule.component
			}
		}
	}
	return "other"
}

func label(f Frame) string {
	if f.Module == "" {
		return f.Func
	}
	return f.Func + " (" + f.Module + ")"
}

func shares(m map[string]int64, total int64, top int) []Share {
	out := make([]Share, 0, len(m))
	for name, n := range m {
		sh := Share{Name: name, Samples: n}
		if total > 0 {
			sh.Percent = 100 * float64(n) / float64(total)
		}
		out = append(out, sh)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Samples != out[j].Samples {
			return out[i].Samples > out[j].Samples
		}
		return out[i].Name < out[j].Name
	})
	if top > 0 && len(out) > top {
		out = out[:top]
	}
	return out
}
