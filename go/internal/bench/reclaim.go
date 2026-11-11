package bench

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ashtonian/TairHash/go/tairhash"
)

// ReclaimConfig is the expiry scenario: load a keyspace whose fields all expire at
// one deadline, then measure how long and how much server time it takes until
// the expired data is gone without any client reading it.
type ReclaimConfig struct {
	// Lease is the time from the start of loading to the shared deadline. It must
	// exceed the load time.
	Lease time.Duration
	// Timeout bounds the wait for reclamation after the deadline.
	Timeout time.Duration
	// Poll is how often key count and server activity are sampled.
	Poll time.Duration
	// SweepEvery and SweepBudget drive Sweeper backends: every SweepEvery the
	// client asks the server to reclaim up to SweepBudget fields.
	SweepEvery  time.Duration
	SweepBudget int
	// MemorySampleKeys is how many logical keys MEMORY USAGE measures.
	MemorySampleKeys int
	Label            string
	DB               int
	ArtifactsDir     string
}

// ReclaimSample is one point of the reclamation timeline.
type ReclaimSample struct {
	AfterDeadlineSec float64 `json:"after_deadline_sec"`
	Keys             int64   `json:"keys"`
	UsedMemory       int64   `json:"used_memory"`
	Utilization      float64 `json:"utilization"`
	ExpiredPerSec    float64 `json:"expired_fields_per_sec"`
}

// ReclaimResult is the outcome of one reclaim scenario.
type ReclaimResult struct {
	Kind      string    `json:"kind"`
	Time      time.Time `json:"time"`
	Label     string    `json:"label"`
	Backend   string    `json:"backend"`
	Keys      int       `json:"keys"`
	Fields    int       `json:"fields_per_key"`
	ValueSize int       `json:"value_size"`
	Lease     string    `json:"lease"`

	LoadSec float64 `json:"load_sec"`
	// BytesPerField is used_memory growth per loaded field, including the
	// backend's expiry index and allocator overhead.
	BytesPerField float64 `json:"bytes_per_field"`
	// DatasetBytesPerField is the growth of used_memory_dataset per field.
	DatasetBytesPerField float64 `json:"dataset_bytes_per_field"`
	// MemoryUsagePerField is MEMORY USAGE (exact) over every physical key of
	// the sampled logical keys, per field.
	MemoryUsagePerField    float64 `json:"memory_usage_per_field"`
	MemoryUsageSampledKeys int     `json:"memory_usage_sampled_keys"`
	LoadedKeys             int64   `json:"loaded_keys"`
	// Load is the server's view of loading; per-op figures are per field written.
	Load ServerMetrics `json:"load_server_metrics"`

	Reclaimed     bool    `json:"reclaimed"`
	ReclaimSec    float64 `json:"reclaim_sec"`
	RemainingKeys int64   `json:"remaining_keys"`
	FieldsPerSec  float64 `json:"fields_per_sec"`
	// Server covers the deadline until the data was gone (or the timeout),
	// including client-driven sweeping; per-op figures are per field reclaimed.
	Server      ServerMetrics   `json:"server_metrics"`
	SweepCalls  int64           `json:"sweep_calls,omitempty"`
	SweptFields int64           `json:"swept_fields,omitempty"`
	Timeline    []ReclaimSample `json:"timeline"`
	Artifacts   string          `json:"artifacts,omitempty"`

	ServerFacts Facts     `json:"server"`
	Host        HostFacts `json:"host"`
}

// Reclaim runs the expiry scenario for one backend.
func Reclaim(ctx context.Context, srv *Server, b Backend, ks *Keyspace, cfg ReclaimConfig) (ReclaimResult, error) {
	res := ReclaimResult{
		Kind: "reclaim", Label: cfg.Label, Backend: b.Name,
		Keys: ks.Keys, Fields: ks.Fields, ValueSize: ks.ValueSize, Lease: cfg.Lease.String(),
	}
	label := "reclaim-" + b.Name
	if cfg.Label != "" {
		label = cfg.Label + "/" + label
	}
	art, err := newArtifacts(cfg.ArtifactsDir, label)
	if err != nil {
		return res, err
	}
	res.Artifacts = art.dir
	if err := srv.Flush(ctx); err != nil {
		return res, err
	}
	if res.ServerFacts, err = srv.Facts(ctx); err != nil {
		return res, err
	}
	res.Host = hostFacts()
	if err := srv.ResetDiagnostics(ctx); err != nil {
		return res, err
	}
	baseline, err := srv.Snapshot(ctx)
	if err != nil {
		return res, err
	}

	start := time.Now()
	deadline := start.Add(cfg.Lease)
	exp := tairhash.ExpireAt(deadline)
	err = forEachField(ctx, ks, func(ctx context.Context, key, field string) error {
		_, err := b.Store.Set(ctx, key, field, ks.value, exp, tairhash.Always)
		return err
	})
	if err != nil {
		return res, err
	}
	res.LoadSec = time.Since(start).Seconds()
	loaded, err := srv.Snapshot(ctx)
	if err != nil {
		return res, err
	}
	if time.Now().After(deadline) {
		return res, fmt.Errorf("load took %.1fs, past the %s lease; raise the lease", res.LoadSec, cfg.Lease)
	}
	fields := float64(ks.Size())
	if res.Load, err = Measure(baseline, loaded, int64(ks.Size()), cfg.DB); err != nil {
		return res, err
	}
	res.LoadedKeys, _ = srv.Keys(ctx)
	res.BytesPerField = float64(res.Load.Memory.UsedMemoryDelta) / fields
	res.DatasetBytesPerField = float64(res.Load.Memory.DatasetDelta) / fields
	if res.MemoryUsagePerField, res.MemoryUsageSampledKeys, err = sampleMemoryUsage(ctx, srv, b, ks, cfg.MemorySampleKeys); err != nil {
		return res, err
	}
	if art.enabled() {
		memStats, err := srv.MemoryStats(ctx)
		if err != nil {
			return res, err
		}
		if err := art.write("memory-stats.loaded.txt", memStats); err != nil {
			return res, err
		}
	}

	select {
	case <-time.After(time.Until(deadline)):
	case <-ctx.Done():
		return res, ctx.Err()
	}
	if err := srv.ResetDiagnostics(ctx); err != nil {
		return res, err
	}
	atDeadline, err := srv.Snapshot(ctx)
	if err != nil {
		return res, err
	}

	sweepCtx, stopSweep := context.WithCancel(ctx)
	var sweepWG sync.WaitGroup
	var sweepErr error
	if b.Sweeper != nil {
		sweepWG.Add(1)
		go func() {
			defer sweepWG.Done()
			tick := time.NewTicker(cfg.SweepEvery)
			defer tick.Stop()
			for {
				n, err := b.Sweeper.Sweep(sweepCtx, cfg.SweepBudget)
				if err != nil {
					if sweepCtx.Err() == nil {
						sweepErr = err
					}
					return
				}
				res.SweepCalls++
				res.SweptFields += n
				select {
				case <-tick.C:
				case <-sweepCtx.Done():
					return
				}
			}
		}()
	}
	halt := func() {
		stopSweep()
		sweepWG.Wait()
	}

	limit := time.Now().Add(cfg.Timeout)
	poll := time.NewTicker(cfg.Poll)
	defer poll.Stop()
	prev, err := srv.quickStats(ctx)
	if err != nil {
		halt()
		return res, err
	}
	prevAt := time.Now()
	for {
		keys, err := srv.Keys(ctx)
		if err != nil {
			halt()
			return res, err
		}
		cur, err := srv.quickStats(ctx)
		if err != nil {
			halt()
			return res, err
		}
		now := time.Now()
		dt := now.Sub(prevAt).Seconds()
		sample := ReclaimSample{AfterDeadlineSec: now.Sub(deadline).Seconds(), Keys: keys, UsedMemory: int64(infoFloat(cur, "used_memory"))}
		if dt > 0 {
			sample.Utilization = (infoFloat(cur, "used_active_time_main_thread") - infoFloat(prev, "used_active_time_main_thread")) / dt
			sample.ExpiredPerSec = (infoFloat(cur, "expired_fields") - infoFloat(prev, "expired_fields")) / dt
		}
		res.Timeline = append(res.Timeline, sample)
		prev, prevAt = cur, now
		if keys == 0 {
			res.Reclaimed = true
			break
		}
		if now.After(limit) {
			res.RemainingKeys = keys
			break
		}
		select {
		case <-poll.C:
		case <-ctx.Done():
			halt()
			return res, ctx.Err()
		}
	}
	res.ReclaimSec = time.Since(deadline).Seconds()
	halt()
	if sweepErr != nil {
		return res, fmt.Errorf("sweeper: %w", sweepErr)
	}

	done, err := srv.Snapshot(ctx)
	if err != nil {
		return res, err
	}
	reclaimed := fields
	if !res.Reclaimed && res.LoadedKeys > 0 {
		// Keys disappear only once all their fields are gone, so this is a
		// lower-bound estimate when reclamation did not finish.
		reclaimed *= 1 - float64(res.RemainingKeys)/float64(res.LoadedKeys)
	}
	if res.Server, err = Measure(atDeadline, done, int64(reclaimed), cfg.DB); err != nil {
		return res, err
	}
	if err := captureDiagnostics(ctx, srv, art, &res.Server); err != nil {
		return res, err
	}
	if reclaimed > 0 && res.ReclaimSec > 0 {
		res.FieldsPerSec = reclaimed / res.ReclaimSec
	}
	res.Time = time.Now().UTC()

	for name, content := range map[string]string{
		"info.baseline.txt":      baseline.Raw,
		"info.loaded.txt":        loaded.Raw,
		"info.deadline.txt":      atDeadline.Raw,
		"info.done.txt":          done.Raw,
		"module-expire.done.txt": done.ModuleExpire,
	} {
		if content == "" {
			continue
		}
		if err := art.write(name, content); err != nil {
			return res, err
		}
	}
	if err := writeJSONLines(art, "timeline.jsonl", res.Timeline); err != nil {
		return res, err
	}
	return res, art.writeJSON("result.json", res)
}

// sampleMemoryUsage measures MEMORY USAGE for up to n logical keys spread over
// the keyspace, summing every physical key behind each, and returns bytes per field.
func sampleMemoryUsage(ctx context.Context, srv *Server, b Backend, ks *Keyspace, n int) (float64, int, error) {
	if n <= 0 {
		return 0, 0, nil
	}
	step := max(1, ks.Keys/n)
	var total int64
	sampled := 0
	for i := 0; i < ks.Keys && sampled < n; i += step {
		for _, k := range b.physicalKeys(ks.keys[i]) {
			bytes, err := srv.MemoryUsage(ctx, k)
			if err != nil {
				return 0, 0, err
			}
			total += bytes
		}
		sampled++
	}
	if sampled == 0 {
		return 0, 0, nil
	}
	return float64(total) / float64(sampled*ks.Fields), sampled, nil
}
