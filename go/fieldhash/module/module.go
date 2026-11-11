// Package module implements fieldhash.VersionedStore on the TairHash module.
package module

import (
	"context"

	"github.com/ashtonian/TairHash/go/tairhash"
)

// Store adapts a tairhash.Client to the fieldhash contracts.
type Store struct {
	c *tairhash.Client
}

// New wraps c.
func New(c *tairhash.Client) *Store {
	return &Store{c: c}
}

// Set writes field=value with EXHSET.
func (s *Store) Set(ctx context.Context, key, field, value string, exp tairhash.Expiry, cond tairhash.Condition) (bool, error) {
	r, err := s.c.Set(ctx, key, field, value, tairhash.SetArgs{Expiry: exp, Condition: cond})
	return r.Applied(), err
}

// SetWithArgs is EXHSET with every option.
func (s *Store) SetWithArgs(ctx context.Context, key, field, value string, args tairhash.SetArgs) (tairhash.SetResult, error) {
	return s.c.Set(ctx, key, field, value, args)
}

// Get reads a field with EXHGET.
func (s *Store) Get(ctx context.Context, key, field string) (string, bool, error) {
	return s.c.Get(ctx, key, field)
}

// GetWithVersion reads a field and its version with EXHGETWITHVER.
func (s *Store) GetWithVersion(ctx context.Context, key, field string) (tairhash.Field, error) {
	return s.c.GetWithVersion(ctx, key, field)
}

// GetAll reads every live field with EXHGETALL.
func (s *Store) GetAll(ctx context.Context, key string) (map[string]string, error) {
	return s.c.GetAll(ctx, key)
}

// Del removes fields with EXHDEL.
func (s *Store) Del(ctx context.Context, key string, fields ...string) (int64, error) {
	return s.c.Del(ctx, key, fields...)
}

// IncrBy increments a field with EXHINCRBY.
func (s *Store) IncrBy(ctx context.Context, key, field string, delta int64, exp tairhash.Expiry) (int64, error) {
	return s.c.IncrBy(ctx, key, field, delta, tairhash.IncrArgs{Expiry: exp})
}

// PTTL reads a field's TTL with EXHPTTL.
func (s *Store) PTTL(ctx context.Context, key, field string) (tairhash.FieldTTL, error) {
	return s.c.PTTL(ctx, key, field)
}
