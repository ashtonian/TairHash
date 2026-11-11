// Package lua implements fieldhash.VersionedStore with a Valkey Functions library
// that reproduces TairHash semantics on a core hash plus a sorted-set expiry index.
// It runs on any Valkey >= 7 without modules; active expiration is client-driven
// through Sweep.
package lua

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/valkey-io/valkey-go"

	"github.com/ashtonian/TairHash/go/tairhash"
)

//go:embed fieldhash.lua
var library string

const (
	indexSuffix = ":exp"
	// maxExact is the largest integer a Lua double represents exactly.
	maxExact = 1<<53 - 1
)

// DataKey and IndexKey return the two keys backing logical key k. They share a
// hash tag so every function call stays within one cluster slot.
func DataKey(k string) string  { return "{" + k + "}" }
func IndexKey(k string) string { return "{" + k + "}" + indexSuffix }

// Store calls the fieldhash function library. It is safe for concurrent use.
type Store struct {
	vk valkey.Client

	sweepMu     sync.Mutex
	sweepCursor uint64
}

// New loads the function library (replacing any previous version) and returns a
// Store using vk. The caller owns vk.
func New(ctx context.Context, vk valkey.Client) (*Store, error) {
	s := &Store{vk: vk}
	if err := s.Load(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Load installs the function library with FUNCTION LOAD REPLACE. Calls reload it
// automatically if the server loses it (FUNCTION FLUSH, restart without persistence).
func (s *Store) Load(ctx context.Context) error {
	if err := s.vk.Do(ctx, s.vk.B().FunctionLoad().Replace().FunctionCode(library).Build()).Error(); err != nil {
		return fmt.Errorf("FUNCTION LOAD fieldhash: %w", err)
	}
	return nil
}

func (s *Store) fcall(ctx context.Context, fn, key string, args ...string) valkey.ValkeyResult {
	d := DataKey(key)
	x := d + indexSuffix
	build := func() valkey.Completed {
		return s.vk.B().Fcall().Function(fn).Numkeys(2).Key(d, x).Arg(args...).Build()
	}
	res := s.vk.Do(ctx, build())
	if isFunctionNotFound(res.Error()) {
		if err := s.Load(ctx); err != nil {
			return res
		}
		res = s.vk.Do(ctx, build())
	}
	return res
}

func isFunctionNotFound(err error) bool {
	ve, ok := valkey.IsValkeyErr(err)
	return ok && strings.Contains(ve.Error(), "Function not found")
}

func opError(fn, key string, err error) error {
	return fmt.Errorf("FCALL %s %q: %w", fn, key, tairhash.MapServerError(err))
}

// Set writes field=value; see fieldhash.Store.
func (s *Store) Set(ctx context.Context, key, field, value string, exp tairhash.Expiry, cond tairhash.Condition) (bool, error) {
	r, err := s.SetWithArgs(ctx, key, field, value, tairhash.SetArgs{Expiry: exp, Condition: cond})
	return r.Applied(), err
}

// SetWithArgs is EXHSET with every option.
func (s *Store) SetWithArgs(ctx context.Context, key, field, value string, args tairhash.SetArgs) (tairhash.SetResult, error) {
	if err := args.Validate(); err != nil {
		return tairhash.NotSet, err
	}
	if err := checkExact("version", args.Version.Version); err != nil {
		return tairhash.NotSet, err
	}
	expMode, expValue := expiryArgs(args.Expiry)
	n, err := s.fcall(ctx, "fh_set", key,
		field, value, expMode, expValue, conditionArg(args.Condition),
		versionModeArg(args.Version.Mode), strconv.FormatInt(args.Version.Version, 10),
	).ToInt64()
	if err != nil {
		return tairhash.NotSet, opError("fh_set", key, err)
	}
	return tairhash.SetResult(n), nil
}

// Get returns a live field's value.
func (s *Store) Get(ctx context.Context, key, field string) (string, bool, error) {
	v, err := s.fcall(ctx, "fh_get", key, field).ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return "", false, nil
		}
		return "", false, opError("fh_get", key, err)
	}
	return v, true, nil
}

// GetWithVersion returns a live field's value and version.
func (s *Store) GetWithVersion(ctx context.Context, key, field string) (tairhash.Field, error) {
	pair, err := s.fcall(ctx, "fh_getwithver", key, field).ToArray()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return tairhash.Field{}, nil
		}
		return tairhash.Field{}, opError("fh_getwithver", key, err)
	}
	if len(pair) != 2 {
		return tairhash.Field{}, fmt.Errorf("FCALL fh_getwithver %q: reply has %d elements, want 2", key, len(pair))
	}
	v, err := pair[0].ToString()
	if err != nil {
		return tairhash.Field{}, fmt.Errorf("FCALL fh_getwithver %q value: %w", key, err)
	}
	ver, err := pair[1].AsInt64()
	if err != nil {
		return tairhash.Field{}, fmt.Errorf("FCALL fh_getwithver %q version: %w", key, err)
	}
	return tairhash.Field{Value: v, Version: ver, Found: true}, nil
}

// GetAll returns every live field.
func (s *Store) GetAll(ctx context.Context, key string) (map[string]string, error) {
	m, err := s.fcall(ctx, "fh_getall", key).AsStrMap()
	if err != nil {
		return nil, opError("fh_getall", key, err)
	}
	return m, nil
}

// Del removes fields and returns how many were live.
func (s *Store) Del(ctx context.Context, key string, fields ...string) (int64, error) {
	if len(fields) == 0 {
		return 0, nil
	}
	n, err := s.fcall(ctx, "fh_del", key, fields...).ToInt64()
	if err != nil {
		return 0, opError("fh_del", key, err)
	}
	return n, nil
}

// IncrBy adds delta to an integer field.
func (s *Store) IncrBy(ctx context.Context, key, field string, delta int64, exp tairhash.Expiry) (int64, error) {
	return s.IncrByWithArgs(ctx, key, field, delta, tairhash.IncrArgs{Expiry: exp})
}

// IncrByWithArgs is EXHINCRBY with every option. Values are exact only within
// ±(2^53-1); results beyond that fail with tairhash.ErrOverflow.
func (s *Store) IncrByWithArgs(ctx context.Context, key, field string, delta int64, args tairhash.IncrArgs) (int64, error) {
	if err := args.Validate(); err != nil {
		return 0, err
	}
	if err := checkExact("delta", delta); err != nil {
		return 0, err
	}
	if err := checkExact("version", args.Version.Version); err != nil {
		return 0, err
	}
	minSet, minVal, err := boundArg("min", args.Min)
	if err != nil {
		return 0, err
	}
	maxSet, maxVal, err := boundArg("max", args.Max)
	if err != nil {
		return 0, err
	}
	expMode, expValue := expiryArgs(args.Expiry)
	n, err := s.fcall(ctx, "fh_incrby", key,
		field, strconv.FormatInt(delta, 10), expMode, expValue,
		versionModeArg(args.Version.Mode), strconv.FormatInt(args.Version.Version, 10),
		minSet, minVal, maxSet, maxVal,
	).ToInt64()
	if err != nil {
		return 0, opError("fh_incrby", key, err)
	}
	return n, nil
}

// PTTL returns a field's remaining lifetime.
func (s *Store) PTTL(ctx context.Context, key, field string) (tairhash.FieldTTL, error) {
	n, err := s.fcall(ctx, "fh_pttl", key, field).ToInt64()
	if err != nil {
		return tairhash.FieldTTL{}, opError("fh_pttl", key, err)
	}
	return tairhash.ParsePTTL(n), nil
}

// Sweep reclaims up to maxFields expired fields. It walks the keyspace with SCAN
// for expiry indexes, resuming where the previous call stopped, which mirrors the
// module's SCAN-mode active expiration. It returns how many fields it removed.
func (s *Store) Sweep(ctx context.Context, maxFields int) (int64, error) {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()

	var removed int64
	for budget := maxFields; budget > 0; {
		entry, err := s.vk.Do(ctx, s.vk.B().Scan().Cursor(s.sweepCursor).Match("{*}"+indexSuffix).Count(1000).Type("zset").Build()).AsScanEntry()
		if err != nil {
			return removed, fmt.Errorf("SCAN for expiry indexes at cursor %d: %w", s.sweepCursor, err)
		}
		for _, idx := range entry.Elements {
			if budget == 0 {
				break
			}
			key := strings.TrimSuffix(idx, indexSuffix)
			key = key[1 : len(key)-1] // strip the hash-tag braces DataKey added
			n, err := s.fcall(ctx, "fh_sweep", key, strconv.Itoa(budget)).ToInt64()
			if err != nil {
				return removed, opError("fh_sweep", key, err)
			}
			removed += n
			budget -= int(n)
		}
		s.sweepCursor = entry.Cursor
		if s.sweepCursor == 0 {
			break
		}
	}
	return removed, nil
}

func expiryArgs(e tairhash.Expiry) (mode, value string) {
	if ms, ok := e.Relative(); ok {
		return "R", strconv.FormatInt(ms, 10)
	}
	if ms, ok := e.Absolute(); ok {
		return "A", strconv.FormatInt(ms, 10)
	}
	if e.IsKeep() {
		return "K", "0"
	}
	return "C", "0"
}

func conditionArg(c tairhash.Condition) string {
	switch c {
	case tairhash.IfNotExists:
		return "NX"
	case tairhash.IfExists:
		return "XX"
	}
	return "ANY"
}

func versionModeArg(m tairhash.VersionMode) string {
	switch m {
	case tairhash.VersionMatch:
		return "VER"
	case tairhash.VersionForce:
		return "ABS"
	case tairhash.VersionAdvance:
		return "GT"
	}
	return "ANY"
}

func boundArg(name string, v *int64) (set, value string, err error) {
	if v == nil {
		return "0", "0", nil
	}
	if err := checkExact(name, *v); err != nil {
		return "", "", err
	}
	return "1", strconv.FormatInt(*v, 10), nil
}

func checkExact(name string, v int64) error {
	if v > maxExact || v < -maxExact {
		return fmt.Errorf("%w: %s %d exceeds the Lua backend's exact integer range ±2^53", tairhash.ErrInvalidArgument, name, v)
	}
	return nil
}
