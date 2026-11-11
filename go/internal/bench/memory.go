package bench

import (
	"context"
	"strings"
	"time"

	"github.com/ashtonian/TairHash/go/tairhash"
)

// LoadMemory is the server's view of loading a keyspace into an empty database.
type LoadMemory struct {
	LoadSec float64 `json:"load_sec"`
	// BytesPerField is used_memory growth per loaded field, including the
	// backend's expiry index and allocator overhead. It is the cross-backend
	// number of record.
	BytesPerField float64 `json:"bytes_per_field"`
	// DatasetBytesPerField is the growth of used_memory_dataset per field.
	DatasetBytesPerField float64 `json:"dataset_bytes_per_field"`
	// MemoryUsagePerField is MEMORY USAGE (exact) over every physical key of
	// the sampled logical keys, per field. For a module key it is whatever the
	// module's mem_usage callback reports.
	MemoryUsagePerField    float64 `json:"memory_usage_per_field"`
	MemoryUsageSampledKeys int     `json:"memory_usage_sampled_keys"`
	// Encoding is OBJECT ENCODING of each physical key behind the first
	// logical key, joined with "+".
	Encoding   string `json:"encoding"`
	LoadedKeys int64  `json:"loaded_keys"`
	// Load is the server's view of loading; per-op figures are per field written.
	Load ServerMetrics `json:"load_server_metrics"`

	baseline, loaded Snapshot
}

// loadMeasured writes every field of ks with exp into the flushed database and
// measures what the data costs.
func loadMeasured(ctx context.Context, srv *Server, b Backend, ks *Keyspace, exp tairhash.Expiry, sampleKeys, db int) (LoadMemory, error) {
	var m LoadMemory
	if err := srv.ResetDiagnostics(ctx); err != nil {
		return m, err
	}
	var err error
	if m.baseline, err = srv.Snapshot(ctx); err != nil {
		return m, err
	}
	start := time.Now()
	if err := preload(ctx, b, ks, exp); err != nil {
		return m, err
	}
	m.LoadSec = time.Since(start).Seconds()
	if m.loaded, err = srv.Snapshot(ctx); err != nil {
		return m, err
	}
	fields := float64(ks.Size())
	if m.Load, err = Measure(m.baseline, m.loaded, int64(ks.Size()), db); err != nil {
		return m, err
	}
	if m.LoadedKeys, err = srv.Keys(ctx); err != nil {
		return m, err
	}
	m.BytesPerField = float64(m.Load.Memory.UsedMemoryDelta) / fields
	m.DatasetBytesPerField = float64(m.Load.Memory.DatasetDelta) / fields
	if m.MemoryUsagePerField, m.MemoryUsageSampledKeys, err = sampleMemoryUsage(ctx, srv, b, ks, sampleKeys); err != nil {
		return m, err
	}
	var encodings []string
	for _, k := range b.physicalKeys(ks.keys[0]) {
		enc, err := srv.ObjectEncoding(ctx, k)
		if err != nil {
			return m, err
		}
		encodings = append(encodings, enc)
	}
	m.Encoding = strings.Join(encodings, "+")
	return m, nil
}

// MemoryConfig is the memory scenario: one load without TTLs and, on backends
// that store them, one with, each into an empty database.
type MemoryConfig struct {
	// TTL is the lease the TTL'd load writes. It must outlast the measurement,
	// so no field expires while it is being counted.
	TTL time.Duration
	// MemorySampleKeys is how many logical keys MEMORY USAGE measures.
	MemorySampleKeys int
	Label            string
	DB               int
	ArtifactsDir     string
	// Placement is recorded in HostFacts.
	Placement string
}

// MemoryResult is one backend's memory per field with and without TTLs. The
// difference is what expiry costs in memory.
type MemoryResult struct {
	Kind        string    `json:"kind"`
	Time        time.Time `json:"time"`
	Label       string    `json:"label"`
	Backend     string    `json:"backend"`
	BackendType string    `json:"backend_type"`
	Keys        int       `json:"keys"`
	Fields      int       `json:"fields_per_key"`
	ValueSize   int       `json:"value_size"`
	TTLLease    string    `json:"ttl_lease"`

	NoTTL LoadMemory `json:"no_ttl"`
	// TTL is nil for a backend that stores no TTLs.
	TTL       *LoadMemory `json:"ttl,omitempty"`
	Artifacts string      `json:"artifacts,omitempty"`

	ServerFacts Facts     `json:"server"`
	Host        HostFacts `json:"host"`
}

// Memory runs the memory scenario for one backend and leaves its database empty.
func Memory(ctx context.Context, srv *Server, b Backend, ks *Keyspace, cfg MemoryConfig) (MemoryResult, error) {
	res := MemoryResult{
		Kind: "memory", Label: cfg.Label, Backend: b.Name, BackendType: b.Type,
		Keys: ks.Keys, Fields: ks.Fields, ValueSize: ks.ValueSize, TTLLease: cfg.TTL.String(),
	}
	art, err := newArtifacts(cfg.ArtifactsDir, cfg.Label, "memory-"+b.Name)
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
	res.Host = hostFacts(cfg.Placement)

	type load struct {
		name string
		exp  tairhash.Expiry
		ttl  bool
	}
	loads := []load{{"nottl", tairhash.NoExpiry(), false}}
	if !b.NoTTL {
		loads = append(loads, load{"ttl", tairhash.TTL(cfg.TTL), true})
	}
	for _, l := range loads {
		m, err := loadMeasured(ctx, srv, b, ks, l.exp, cfg.MemorySampleKeys, cfg.DB)
		if err != nil {
			return res, err
		}
		if art.enabled() {
			memStats, err := srv.MemoryStats(ctx)
			if err != nil {
				return res, err
			}
			for name, content := range map[string]string{
				"memory-stats." + l.name + ".txt":  memStats,
				"info.baseline." + l.name + ".txt": m.baseline.Raw,
				"info.loaded." + l.name + ".txt":   m.loaded.Raw,
			} {
				if err := art.write(name, content); err != nil {
					return res, err
				}
			}
		}
		if l.ttl {
			res.TTL = &m
		} else {
			res.NoTTL = m
		}
		if err := srv.Flush(ctx); err != nil {
			return res, err
		}
	}
	res.Time = time.Now().UTC()
	return res, art.writeJSON("result.json", res)
}
