// Package bench drives identical workloads against fieldhash backends and records
// client latency alongside the server's own cost per operation.
package bench

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/ashtonian/TairHash/go/fieldhash"
	"github.com/ashtonian/TairHash/go/presence"
	"github.com/ashtonian/TairHash/go/tairhash"
)

// Backend is one implementation under test.
type Backend struct {
	Name  string
	Store fieldhash.Store
	// Versioned is nil when the backend has no per-field versions.
	Versioned fieldhash.VersionedStore
	// Sweeper is nil when the server reclaims expired fields itself.
	Sweeper fieldhash.Sweeper
	// PhysicalKeys maps a logical key to the server keys that store it; nil
	// means the logical key is the only one.
	PhysicalKeys func(key string) []string
}

func (b Backend) physicalKeys(key string) []string {
	if b.PhysicalKeys == nil {
		return []string{key}
	}
	return b.PhysicalKeys(key)
}

// Keyspace is the data set a workload touches. Names are built once so the hot
// loop does no formatting.
type Keyspace struct {
	Keys      int
	Fields    int
	ValueSize int
	// TTL is the lease written fields get. It must outlast the run so steady-state
	// workloads measure access cost, not expiry; expiry has its own scenario.
	TTL time.Duration

	keys   []string
	fields []string
	value  string
}

// NewKeyspace precomputes names and a deterministic value of valueSize bytes.
func NewKeyspace(prefix string, keys, fields, valueSize int, ttl time.Duration) (*Keyspace, error) {
	if keys <= 0 || fields <= 0 || valueSize <= 0 {
		return nil, fmt.Errorf("keyspace needs positive keys, fields and value size; got %d/%d/%d", keys, fields, valueSize)
	}
	ks := &Keyspace{Keys: keys, Fields: fields, ValueSize: valueSize, TTL: ttl}
	ks.keys = make([]string, keys)
	for i := range ks.keys {
		ks.keys[i] = prefix + strconv.Itoa(i)
	}
	ks.fields = make([]string, fields)
	for i := range ks.fields {
		ks.fields[i] = "f" + strconv.Itoa(i)
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	rng := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, valueSize)
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	ks.value = string(b)
	return ks, nil
}

// Size is the number of fields in the keyspace.
func (ks *Keyspace) Size() int { return ks.Keys * ks.Fields }

// Worker is per-goroutine state. Workloads must not share it.
type Worker struct {
	rng       *rand.Rand
	conflicts int64
}

func (w *Worker) pick(ks *Keyspace) (key, field string) {
	return ks.keys[w.rng.IntN(ks.Keys)], ks.fields[w.rng.IntN(ks.Fields)]
}

// Workload is one access pattern.
type Workload struct {
	Name        string
	Description string
	// NeedsVersions restricts the workload to backends with Versioned.
	NeedsVersions bool
	// Prepare builds per-run state and writes the data the workload reads. It
	// returns the operation to measure.
	Prepare func(ctx context.Context, b Backend, ks *Keyspace) (Op, error)
}

// Op performs one logical operation.
type Op func(ctx context.Context, w *Worker) error

// Workloads is every registered workload, in report order.
var Workloads = []Workload{
	{
		Name:        "set-ttl",
		Description: "overwrite an existing field and renew its TTL (EXHSET k f v PX ttl)",
		Prepare: func(ctx context.Context, b Backend, ks *Keyspace) (Op, error) {
			if err := preload(ctx, b, ks); err != nil {
				return nil, err
			}
			exp := tairhash.TTL(ks.TTL)
			return func(ctx context.Context, w *Worker) error {
				key, field := w.pick(ks)
				_, err := b.Store.Set(ctx, key, field, ks.value, exp, tairhash.Always)
				return err
			}, nil
		},
	},
	{
		Name:        "get",
		Description: "read one live field that has a TTL (EXHGET)",
		Prepare: func(ctx context.Context, b Backend, ks *Keyspace) (Op, error) {
			if err := preload(ctx, b, ks); err != nil {
				return nil, err
			}
			return func(ctx context.Context, w *Worker) error {
				key, field := w.pick(ks)
				_, found, err := b.Store.Get(ctx, key, field)
				if err == nil && !found {
					err = fmt.Errorf("%w: %s/%s", errMissing, key, field)
				}
				return err
			}, nil
		},
	},
	{
		Name:        "getall",
		Description: "read every field of one hash (EXHGETALL)",
		Prepare: func(ctx context.Context, b Backend, ks *Keyspace) (Op, error) {
			if err := preload(ctx, b, ks); err != nil {
				return nil, err
			}
			return func(ctx context.Context, w *Worker) error {
				key := ks.keys[w.rng.IntN(ks.Keys)]
				all, err := b.Store.GetAll(ctx, key)
				if err == nil && len(all) != ks.Fields {
					err = fmt.Errorf("%w: %s has %d fields, want %d", errMissing, key, len(all), ks.Fields)
				}
				return err
			}, nil
		},
	},
	{
		Name:        "incr-ttl",
		Description: "increment a counter and renew its TTL (EXHINCRBY k f 1 PX ttl)",
		Prepare: func(ctx context.Context, b Backend, ks *Keyspace) (Op, error) {
			exp := tairhash.TTL(ks.TTL)
			err := forEachField(ctx, ks, func(ctx context.Context, key, field string) error {
				_, err := b.Store.Set(ctx, key, field, "0", exp, tairhash.Always)
				return err
			})
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, w *Worker) error {
				key, field := w.pick(ks)
				_, err := b.Store.IncrBy(ctx, key, field, 1, exp)
				return err
			}, nil
		},
	},
	{
		Name:        "mixed",
		Description: "80% get / 20% set-ttl",
		Prepare: func(ctx context.Context, b Backend, ks *Keyspace) (Op, error) {
			if err := preload(ctx, b, ks); err != nil {
				return nil, err
			}
			exp := tairhash.TTL(ks.TTL)
			return func(ctx context.Context, w *Worker) error {
				key, field := w.pick(ks)
				if w.rng.IntN(10) < 2 {
					_, err := b.Store.Set(ctx, key, field, ks.value, exp, tairhash.Always)
					return err
				}
				_, _, err := b.Store.Get(ctx, key, field)
				return err
			}, nil
		},
	},
	{
		Name:        "presence",
		Description: "presence registry: 90% heartbeat, 9% list a user's devices, 1% disconnect",
		Prepare: func(ctx context.Context, b Backend, ks *Keyspace) (Op, error) {
			reg, err := presence.New(b.Store, presence.WithTTL(ks.TTL), presence.WithKeyPrefix(""))
			if err != nil {
				return nil, err
			}
			session := func(device string) presence.Session {
				return presence.Session{Device: device, Node: "gw-1", Status: "active"}
			}
			err = forEachField(ctx, ks, func(ctx context.Context, user, device string) error {
				return reg.Heartbeat(ctx, user, session(device))
			})
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, w *Worker) error {
				user, device := w.pick(ks)
				switch n := w.rng.IntN(100); {
				case n < 90:
					return reg.Heartbeat(ctx, user, session(device))
				case n < 99:
					_, err := reg.Online(ctx, user)
					return err
				default:
					_, err := reg.Disconnect(ctx, user, device)
					return err
				}
			}, nil
		},
	},
	{
		Name:          "cas",
		Description:   "optimistic update: read value+version, write back with VER, retry on conflict",
		NeedsVersions: true,
		Prepare: func(ctx context.Context, b Backend, ks *Keyspace) (Op, error) {
			if err := preload(ctx, b, ks); err != nil {
				return nil, err
			}
			exp := tairhash.TTL(ks.TTL)
			return func(ctx context.Context, w *Worker) error {
				key, field := w.pick(ks)
				for attempt := 0; attempt < maxCASAttempts; attempt++ {
					cur, err := b.Versioned.GetWithVersion(ctx, key, field)
					if err != nil {
						return err
					}
					if !cur.Found {
						return fmt.Errorf("%w: %s/%s", errMissing, key, field)
					}
					_, err = b.Versioned.SetWithArgs(ctx, key, field, ks.value, tairhash.SetArgs{
						Expiry:  exp,
						Version: tairhash.MatchVersion(cur.Version),
					})
					if !errors.Is(err, tairhash.ErrStaleVersion) {
						return err
					}
					w.conflicts++
				}
				return fmt.Errorf("cas %s/%s: gave up after %d conflicts", key, field, maxCASAttempts)
			}, nil
		},
	},
}

const maxCASAttempts = 100

var errMissing = errors.New("bench: preloaded data missing")

// LookupWorkload returns the named workload.
func LookupWorkload(name string) (Workload, bool) {
	for _, w := range Workloads {
		if w.Name == name {
			return w, true
		}
	}
	return Workload{}, false
}

func preload(ctx context.Context, b Backend, ks *Keyspace) error {
	exp := tairhash.TTL(ks.TTL)
	return forEachField(ctx, ks, func(ctx context.Context, key, field string) error {
		_, err := b.Store.Set(ctx, key, field, ks.value, exp, tairhash.Always)
		return err
	})
}
