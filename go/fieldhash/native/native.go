// Package native implements fieldhash.Store with Valkey 9 hash-field expiration.
// The server expires fields itself; there are no per-field versions.
package native

import (
	"context"
	"fmt"
	"strconv"

	"github.com/valkey-io/valkey-go"

	"github.com/ashtonian/TairHash/go/tairhash"
)

// Store issues native hash commands. It is safe for concurrent use.
type Store struct {
	vk valkey.Client
}

// New wraps vk. The caller owns vk.
func New(vk valkey.Client) *Store {
	return &Store{vk: vk}
}

// coreErrors maps core hash-command replies that word TairHash conditions differently.
var coreErrors = map[string]error{
	"hash value is not an integer": tairhash.ErrNotInteger,
}

func opError(cmd, key string, err error) error {
	return fmt.Errorf("%s %q: %w", cmd, key, tairhash.MapServerErrorWith(err, coreErrors))
}

// Set writes field=value with HSETEX. Without an expiry HSETEX clears the field's
// TTL, matching EXHSET.
func (s *Store) Set(ctx context.Context, key, field, value string, exp tairhash.Expiry, cond tairhash.Condition) (bool, error) {
	if err := exp.Validate(); err != nil {
		return false, err
	}
	if err := cond.Validate(); err != nil {
		return false, err
	}
	// The typed HSETEX builder forks into a distinct type per option branch, so
	// the 3x4 condition/expiry matrix is assembled as arguments instead.
	args := make([]string, 0, 8)
	switch cond {
	case tairhash.IfNotExists:
		args = append(args, "FNX")
	case tairhash.IfExists:
		args = append(args, "FXX")
	}
	switch {
	case exp.IsKeep():
		args = append(args, "KEEPTTL")
	case !exp.IsClear():
		if ms, ok := exp.Relative(); ok {
			args = append(args, "PX", strconv.FormatInt(ms, 10))
		} else if at, ok := exp.Absolute(); ok {
			args = append(args, "PXAT", strconv.FormatInt(at, 10))
		}
	}
	args = append(args, "FIELDS", "1", field, value)
	n, err := s.vk.Do(ctx, s.vk.B().Arbitrary("HSETEX").Keys(key).Args(args...).Build()).ToInt64()
	if err != nil {
		return false, opError("HSETEX", key, err)
	}
	return n == 1, nil
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

// GetAll reads every live field with HGETALL.
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

// IncrBy increments a field with HINCRBY. HINCRBY preserves a field's TTL, so any
// other expiry runs HPERSIST/HPEXPIRE/HPEXPIREAT in the same MULTI to match EXHINCRBY.
func (s *Store) IncrBy(ctx context.Context, key, field string, delta int64, exp tairhash.Expiry) (int64, error) {
	if err := exp.Validate(); err != nil {
		return 0, err
	}
	incr := s.vk.B().Hincrby().Key(key).Field(field).Increment(delta).Build()
	if exp.IsKeep() {
		n, err := s.vk.Do(ctx, incr).ToInt64()
		if err != nil {
			return 0, opError("HINCRBY", key, err)
		}
		return n, nil
	}

	var ttl valkey.Completed
	switch {
	case exp.IsClear():
		ttl = s.vk.B().Hpersist().Key(key).Fields().Numfields(1).Field(field).Build()
	default:
		if ms, ok := exp.Relative(); ok {
			ttl = s.vk.B().Hpexpire().Key(key).Milliseconds(ms).Fields().Numfields(1).Field(field).Build()
		} else {
			at, _ := exp.Absolute()
			ttl = s.vk.B().Hpexpireat().Key(key).UnixTimeMilliseconds(at).Fields().Numfields(1).Field(field).Build()
		}
	}
	res := s.vk.DoMulti(ctx, s.vk.B().Multi().Build(), incr, ttl, s.vk.B().Exec().Build())
	replies, err := res[3].ToArray()
	if err != nil {
		for _, r := range res[:3] {
			if rerr := r.Error(); rerr != nil {
				return 0, opError("MULTI HINCRBY", key, rerr)
			}
		}
		return 0, opError("EXEC HINCRBY", key, err)
	}
	if len(replies) != 2 {
		return 0, fmt.Errorf("EXEC HINCRBY %q: reply has %d elements, want 2", key, len(replies))
	}
	n, err := replies[0].AsInt64()
	if err != nil {
		return 0, opError("HINCRBY", key, err)
	}
	return n, nil
}

// PTTL reads a field's TTL with HPTTL.
func (s *Store) PTTL(ctx context.Context, key, field string) (tairhash.FieldTTL, error) {
	ns, err := s.vk.Do(ctx, s.vk.B().Hpttl().Key(key).Fields().Numfields(1).Field(field).Build()).AsIntSlice()
	if err != nil {
		return tairhash.FieldTTL{}, opError("HPTTL", key, err)
	}
	if len(ns) != 1 {
		return tairhash.FieldTTL{}, fmt.Errorf("HPTTL %q: reply has %d elements, want 1", key, len(ns))
	}
	// HPTTL uses -2 for a missing key or field; ParsePTTL folds every negative
	// other than -1 into "absent".
	return tairhash.ParsePTTL(ns[0]), nil
}
