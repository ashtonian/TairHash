// Package fieldhash is the contract shared by stores that keep a hash whose fields
// expire independently. Three implementations live in subpackages so the same
// workload can run against each:
//
//   - module: the TairHash module through the tairhash driver.
//   - lua:    TairHash semantics emulated by a Valkey Functions library on core types.
//   - native: Valkey 9 hash-field expiration (HSETEX/HPEXPIRE/...); no versions.
//
// Semantics follow TairHash: a write without an expiry clears the field's TTL,
// TTLs are measured on the server clock, and a field whose deadline has passed is
// invisible to every read.
package fieldhash

import (
	"context"

	"github.com/ashtonian/TairHash/go/tairhash"
)

// Store is a hash with per-field TTLs.
type Store interface {
	// Set writes field=value and reports whether cond allowed the write.
	Set(ctx context.Context, key, field, value string, exp tairhash.Expiry, cond tairhash.Condition) (bool, error)
	// Get returns a live field's value.
	Get(ctx context.Context, key, field string) (string, bool, error)
	// GetAll returns every live field.
	GetAll(ctx context.Context, key string) (map[string]string, error)
	// Del removes fields and returns how many were live.
	Del(ctx context.Context, key string, fields ...string) (int64, error)
	// IncrBy adds delta to an integer field (absent counts as 0) and applies exp.
	IncrBy(ctx context.Context, key, field string, delta int64, exp tairhash.Expiry) (int64, error)
	// PTTL returns a field's remaining lifetime.
	PTTL(ctx context.Context, key, field string) (tairhash.FieldTTL, error)
}

// VersionedStore adds TairHash's per-field versions for optimistic concurrency.
type VersionedStore interface {
	Store
	// SetWithArgs is the full EXHSET: expiry, existence condition and version check.
	SetWithArgs(ctx context.Context, key, field, value string, args tairhash.SetArgs) (tairhash.SetResult, error)
	// GetWithVersion returns a live field's value and version.
	GetWithVersion(ctx context.Context, key, field string) (tairhash.Field, error)
}

// Sweeper is implemented by stores whose active expiration is driven by the
// client rather than the server.
type Sweeper interface {
	// Sweep reclaims up to maxFields expired fields and returns how many it removed.
	Sweep(ctx context.Context, maxFields int) (int64, error)
}
