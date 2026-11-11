// Package plainhash is the benchmark's plain-hash baseline: Valkey's core hash
// commands (HSET, HGET, HGETALL, HDEL, HINCRBY) with no expiry and no versions.
// It implements fieldhash.Store so the same workloads drive it, but only the
// subset of the contract a plain hash can honour: writes must not set a TTL and
// IfExists is unsupported. It is not a fieldhash backend and is not part of the
// conformance suite.
package plainhash

import (
	"context"
	"errors"
	"fmt"

	"github.com/valkey-io/valkey-go"

	"github.com/ashtonian/TairHash/go/tairhash"
)

// ErrUnsupported reports a request a plain hash cannot express.
var ErrUnsupported = errors.New("plainhash: unsupported")

// Store issues core hash commands. It is safe for concurrent use.
type Store struct {
	vk valkey.Client
}

// New wraps vk. The caller owns vk.
func New(vk valkey.Client) *Store {
	return &Store{vk: vk}
}

var coreErrors = map[string]error{
	"hash value is not an integer": tairhash.ErrNotInteger,
}

func opError(cmd, key string, err error) error {
	return fmt.Errorf("%s %q: %w", cmd, key, tairhash.MapServerErrorWith(err, coreErrors))
}

// noTTL accepts the expiries that mean nothing without TTLs: clear (the zero
// value) and keep.
func noTTL(op string, exp tairhash.Expiry) error {
	if exp.IsClear() || exp.IsKeep() {
		return nil
	}
	return fmt.Errorf("%w: %s with a TTL", ErrUnsupported, op)
}

// Set writes field=value with HSET, or HSETNX for IfNotExists.
func (s *Store) Set(ctx context.Context, key, field, value string, exp tairhash.Expiry, cond tairhash.Condition) (bool, error) {
	if err := noTTL("set", exp); err != nil {
		return false, err
	}
	switch cond {
	case tairhash.Always:
		if err := s.vk.Do(ctx, s.vk.B().Hset().Key(key).FieldValue().FieldValue(field, value).Build()).Error(); err != nil {
			return false, opError("HSET", key, err)
		}
		return true, nil
	case tairhash.IfNotExists:
		ok, err := s.vk.Do(ctx, s.vk.B().Hsetnx().Key(key).Field(field).Value(value).Build()).AsBool()
		if err != nil {
			return false, opError("HSETNX", key, err)
		}
		return ok, nil
	default:
		return false, fmt.Errorf("%w: set with condition %d", ErrUnsupported, cond)
	}
}

// Get reads a field with HGET.
func (s *Store) Get(ctx context.Context, key, field string) (string, bool, error) {
	v, err := s.vk.Do(ctx, s.vk.B().Hget().Key(key).Field(field).Build()).ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return "", false, nil
		}
		return "", false, opError("HGET", key, err)
	}
	return v, true, nil
}

// GetAll reads every field with HGETALL.
func (s *Store) GetAll(ctx context.Context, key string) (map[string]string, error) {
	m, err := s.vk.Do(ctx, s.vk.B().Hgetall().Key(key).Build()).AsStrMap()
	if err != nil {
		return nil, opError("HGETALL", key, err)
	}
	return m, nil
}

// Del removes fields with HDEL.
func (s *Store) Del(ctx context.Context, key string, fields ...string) (int64, error) {
	if len(fields) == 0 {
		return 0, nil
	}
	n, err := s.vk.Do(ctx, s.vk.B().Hdel().Key(key).Field(fields...).Build()).ToInt64()
	if err != nil {
		return 0, opError("HDEL", key, err)
	}
	return n, nil
}

// IncrBy increments a field with HINCRBY.
func (s *Store) IncrBy(ctx context.Context, key, field string, delta int64, exp tairhash.Expiry) (int64, error) {
	if err := noTTL("incrby", exp); err != nil {
		return 0, err
	}
	n, err := s.vk.Do(ctx, s.vk.B().Hincrby().Key(key).Field(field).Increment(delta).Build()).ToInt64()
	if err != nil {
		return 0, opError("HINCRBY", key, err)
	}
	return n, nil
}

// PTTL reports whether the field exists; plain-hash fields never expire.
func (s *Store) PTTL(ctx context.Context, key, field string) (tairhash.FieldTTL, error) {
	ok, err := s.vk.Do(ctx, s.vk.B().Hexists().Key(key).Field(field).Build()).AsBool()
	if err != nil {
		return tairhash.FieldTTL{}, opError("HEXISTS", key, err)
	}
	return tairhash.FieldTTL{Exists: ok}, nil
}
