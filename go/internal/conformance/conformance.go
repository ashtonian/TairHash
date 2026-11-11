// Package conformance is the behavioural contract every fieldhash backend must
// pass. The TairHash module is the reference: the suite is first validated
// against it, then run unchanged against the emulations, which is what makes the
// benchmark an apples-to-apples comparison.
//
// Expiry is exercised with deadlines already in the past and with hour-long TTLs,
// never by sleeping, so results do not depend on scheduling.
package conformance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashtonian/TairHash/go/fieldhash"
	"github.com/ashtonian/TairHash/go/internal/testserver"
	"github.com/ashtonian/TairHash/go/tairhash"
)

var (
	past = tairhash.ExpireAt(time.UnixMilli(1))
	hour = tairhash.TTL(time.Hour)
)

// RunStore checks fieldhash.Store semantics.
func RunStore(t *testing.T, s fieldhash.Store) {
	ctx := testserver.Context(t)

	t.Run("set then read", func(t *testing.T) {
		ok, err := s.Set(ctx, "rw", "f", "v1", tairhash.NoExpiry(), tairhash.Always)
		require.NoError(t, err)
		assert.True(t, ok)

		v, found, err := s.Get(ctx, "rw", "f")
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, "v1", v)

		all, err := s.GetAll(ctx, "rw")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"f": "v1"}, all)
	})

	t.Run("missing key and field", func(t *testing.T) {
		_, found, err := s.Get(ctx, "nokey", "f")
		require.NoError(t, err)
		assert.False(t, found)

		all, err := s.GetAll(ctx, "nokey")
		require.NoError(t, err)
		assert.Empty(t, all)

		ttl, err := s.PTTL(ctx, "nokey", "f")
		require.NoError(t, err)
		assert.False(t, ttl.Exists)

		_, err = s.Set(ctx, "somekey", "a", "v", tairhash.NoExpiry(), tairhash.Always)
		require.NoError(t, err)
		ttl, err = s.PTTL(ctx, "somekey", "missing")
		require.NoError(t, err)
		assert.False(t, ttl.Exists)
	})

	t.Run("existence conditions", func(t *testing.T) {
		ok, err := s.Set(ctx, "cond", "f", "first", tairhash.NoExpiry(), tairhash.IfNotExists)
		require.NoError(t, err)
		assert.True(t, ok, "NX on absent field writes")

		ok, err = s.Set(ctx, "cond", "f", "second", tairhash.NoExpiry(), tairhash.IfNotExists)
		require.NoError(t, err)
		assert.False(t, ok, "NX on present field is refused")

		ok, err = s.Set(ctx, "cond", "g", "x", tairhash.NoExpiry(), tairhash.IfExists)
		require.NoError(t, err)
		assert.False(t, ok, "XX on absent field is refused")

		ok, err = s.Set(ctx, "cond", "f", "third", tairhash.NoExpiry(), tairhash.IfExists)
		require.NoError(t, err)
		assert.True(t, ok, "XX on present field writes")

		all, err := s.GetAll(ctx, "cond")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"f": "third"}, all)
	})

	t.Run("ttl lifecycle", func(t *testing.T) {
		_, err := s.Set(ctx, "ttl", "f", "v", hour, tairhash.Always)
		require.NoError(t, err)
		assertTTL(t, s, "ttl", "f", true)

		_, err = s.Set(ctx, "ttl", "f", "v2", tairhash.KeepTTL(), tairhash.Always)
		require.NoError(t, err)
		assertTTL(t, s, "ttl", "f", true)

		_, err = s.Set(ctx, "ttl", "f", "v3", tairhash.NoExpiry(), tairhash.Always)
		require.NoError(t, err)
		assertTTL(t, s, "ttl", "f", false)

		at := time.Now().Add(time.Hour)
		_, err = s.Set(ctx, "ttl", "f", "v4", tairhash.ExpireAt(at), tairhash.Always)
		require.NoError(t, err)
		assertTTL(t, s, "ttl", "f", true)
	})

	t.Run("expired fields are invisible", func(t *testing.T) {
		_, err := s.Set(ctx, "exp", "live", "v", hour, tairhash.Always)
		require.NoError(t, err)
		_, err = s.Set(ctx, "exp", "dead", "v", past, tairhash.Always)
		require.NoError(t, err)
		_, err = s.Set(ctx, "exp", "now", "v", tairhash.TTL(0), tairhash.Always)
		require.NoError(t, err)

		for _, f := range []string{"dead", "now"} {
			_, found, err := s.Get(ctx, "exp", f)
			require.NoError(t, err)
			assert.False(t, found, f)
			ttl, err := s.PTTL(ctx, "exp", f)
			require.NoError(t, err)
			assert.False(t, ttl.Exists, f)
		}
		all, err := s.GetAll(ctx, "exp")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"live": "v"}, all)
	})

	t.Run("expired field can be recreated with NX", func(t *testing.T) {
		_, err := s.Set(ctx, "renx", "f", "old", past, tairhash.Always)
		require.NoError(t, err)
		ok, err := s.Set(ctx, "renx", "f", "new", tairhash.NoExpiry(), tairhash.IfNotExists)
		require.NoError(t, err)
		assert.True(t, ok)
		v, _, err := s.Get(ctx, "renx", "f")
		require.NoError(t, err)
		assert.Equal(t, "new", v)
	})

	t.Run("del counts live fields", func(t *testing.T) {
		_, err := s.Set(ctx, "del", "live", "v", tairhash.NoExpiry(), tairhash.Always)
		require.NoError(t, err)
		_, err = s.Set(ctx, "del", "keep", "v", tairhash.NoExpiry(), tairhash.Always)
		require.NoError(t, err)
		_, err = s.Set(ctx, "del", "dead", "v", past, tairhash.Always)
		require.NoError(t, err)

		n, err := s.Del(ctx, "del", "live", "dead", "missing")
		require.NoError(t, err)
		assert.Equal(t, int64(1), n)

		all, err := s.GetAll(ctx, "del")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"keep": "v"}, all)
	})

	t.Run("incrby", func(t *testing.T) {
		n, err := s.IncrBy(ctx, "ctr", "c", 5, hour)
		require.NoError(t, err)
		assert.Equal(t, int64(5), n)
		assertTTL(t, s, "ctr", "c", true)

		n, err = s.IncrBy(ctx, "ctr", "c", -7, tairhash.KeepTTL())
		require.NoError(t, err)
		assert.Equal(t, int64(-2), n)
		assertTTL(t, s, "ctr", "c", true)

		n, err = s.IncrBy(ctx, "ctr", "c", 2, tairhash.NoExpiry())
		require.NoError(t, err)
		assert.Equal(t, int64(0), n)
		assertTTL(t, s, "ctr", "c", false)

		_, err = s.Set(ctx, "ctr", "s", "abc", tairhash.NoExpiry(), tairhash.Always)
		require.NoError(t, err)
		_, err = s.IncrBy(ctx, "ctr", "s", 1, tairhash.NoExpiry())
		assert.ErrorIs(t, err, tairhash.ErrNotInteger)
	})

	t.Run("incrby restarts an expired counter", func(t *testing.T) {
		_, err := s.IncrBy(ctx, "ctrexp", "c", 41, past)
		require.NoError(t, err)
		n, err := s.IncrBy(ctx, "ctrexp", "c", 1, hour)
		require.NoError(t, err)
		assert.Equal(t, int64(1), n)
	})

	t.Run("binary-safe values", func(t *testing.T) {
		v := "a\x00b\r\n\xff"
		_, err := s.Set(ctx, "bin", "f\x00", v, hour, tairhash.Always)
		require.NoError(t, err)
		got, found, err := s.Get(ctx, "bin", "f\x00")
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, v, got)
	})
}

// RunVersioned checks TairHash version semantics.
func RunVersioned(t *testing.T, s fieldhash.VersionedStore) {
	ctx := testserver.Context(t)

	version := func(t *testing.T, key, field string) int64 {
		t.Helper()
		f, err := s.GetWithVersion(ctx, key, field)
		require.NoError(t, err)
		require.True(t, f.Found, "%s/%s", key, field)
		return f.Version
	}
	set := func(t *testing.T, key, field, value string, args tairhash.SetArgs) (tairhash.SetResult, error) {
		t.Helper()
		return s.SetWithArgs(ctx, key, field, value, args)
	}

	t.Run("versions increment on write", func(t *testing.T) {
		r, err := set(t, "ver", "f", "a", tairhash.SetArgs{})
		require.NoError(t, err)
		assert.Equal(t, tairhash.Created, r)
		assert.Equal(t, int64(1), version(t, "ver", "f"))

		r, err = set(t, "ver", "f", "b", tairhash.SetArgs{})
		require.NoError(t, err)
		assert.Equal(t, tairhash.Updated, r)
		assert.Equal(t, int64(2), version(t, "ver", "f"))

		f, err := s.GetWithVersion(ctx, "ver", "f")
		require.NoError(t, err)
		assert.Equal(t, tairhash.Field{Value: "b", Version: 2, Found: true}, f)
	})

	t.Run("match version", func(t *testing.T) {
		_, err := set(t, "cas", "f", "a", tairhash.SetArgs{})
		require.NoError(t, err)

		_, err = set(t, "cas", "f", "x", tairhash.SetArgs{Version: tairhash.MatchVersion(7)})
		assert.ErrorIs(t, err, tairhash.ErrStaleVersion)

		r, err := set(t, "cas", "f", "b", tairhash.SetArgs{Version: tairhash.MatchVersion(1)})
		require.NoError(t, err)
		assert.Equal(t, tairhash.Updated, r)
		assert.Equal(t, int64(2), version(t, "cas", "f"))

		_, err = set(t, "cas", "f", "c", tairhash.SetArgs{Version: tairhash.MatchVersion(0)})
		require.NoError(t, err, "version 0 disables the check")
		assert.Equal(t, int64(3), version(t, "cas", "f"))

		r, err = set(t, "cas", "new", "n", tairhash.SetArgs{Version: tairhash.MatchVersion(99)})
		require.NoError(t, err, "absent fields always match")
		assert.Equal(t, tairhash.Created, r)
		assert.Equal(t, int64(1), version(t, "cas", "new"))
	})

	t.Run("force and advance version", func(t *testing.T) {
		_, err := set(t, "abs", "f", "a", tairhash.SetArgs{Version: tairhash.ForceVersion(10)})
		require.NoError(t, err)
		assert.Equal(t, int64(10), version(t, "abs", "f"))

		_, err = set(t, "abs", "f", "b", tairhash.SetArgs{Version: tairhash.AdvanceVersion(10)})
		assert.ErrorIs(t, err, tairhash.ErrStaleVersion)

		_, err = set(t, "abs", "f", "c", tairhash.SetArgs{Version: tairhash.AdvanceVersion(15)})
		require.NoError(t, err)
		assert.Equal(t, int64(15), version(t, "abs", "f"))

		_, err = set(t, "abs", "f", "d", tairhash.SetArgs{Version: tairhash.ForceVersion(3)})
		require.NoError(t, err)
		assert.Equal(t, int64(3), version(t, "abs", "f"))
	})

	t.Run("condition is checked before version", func(t *testing.T) {
		_, err := set(t, "order", "f", "a", tairhash.SetArgs{})
		require.NoError(t, err)
		r, err := set(t, "order", "f", "b", tairhash.SetArgs{Condition: tairhash.IfNotExists, Version: tairhash.MatchVersion(42)})
		require.NoError(t, err)
		assert.Equal(t, tairhash.NotSet, r)
	})

	t.Run("expired field restarts at version 1", func(t *testing.T) {
		_, err := set(t, "verexp", "f", "a", tairhash.SetArgs{Version: tairhash.ForceVersion(50), Expiry: past})
		require.NoError(t, err)
		r, err := set(t, "verexp", "f", "b", tairhash.SetArgs{})
		require.NoError(t, err)
		assert.Equal(t, tairhash.Created, r)
		assert.Equal(t, int64(1), version(t, "verexp", "f"))
	})

	t.Run("missing field has no version", func(t *testing.T) {
		f, err := s.GetWithVersion(ctx, "verexp", "missing")
		require.NoError(t, err)
		assert.False(t, f.Found)
	})
}

func assertTTL(t *testing.T, s fieldhash.Store, key, field string, expires bool) {
	t.Helper()
	ttl, err := s.PTTL(testserver.Context(t), key, field)
	require.NoError(t, err)
	require.True(t, ttl.Exists, "%s/%s exists", key, field)
	assert.Equal(t, expires, ttl.Expires, "%s/%s has TTL", key, field)
	if expires {
		assert.Greater(t, ttl.Remaining, time.Duration(0))
		assert.LessOrEqual(t, ttl.Remaining, time.Hour)
	}
}
