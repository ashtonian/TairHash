package lua

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashtonian/TairHash/go/internal/conformance"
	"github.com/ashtonian/TairHash/go/internal/testserver"
	"github.com/ashtonian/TairHash/go/tairhash"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(testserver.Context(t), testserver.Client(t, testserver.DBLua))
	require.NoError(t, err)
	return s
}

func TestConformance(t *testing.T) {
	s := newStore(t)
	t.Run("store", func(t *testing.T) { conformance.RunStore(t, s) })
	t.Run("versioned", func(t *testing.T) { conformance.RunVersioned(t, s) })
}

func TestPassiveExpiryOnWrite(t *testing.T) {
	s := newStore(t)
	ctx := testserver.Context(t)
	past := tairhash.ExpireAt(time.UnixMilli(1))

	for i := range 5 {
		_, err := s.Set(ctx, "k", fmt.Sprintf("dead%d", i), "v", past, tairhash.Always)
		require.NoError(t, err)
	}
	// Every write first reclaims due fields from the index head, so each expired
	// write above reclaimed its predecessor and the first live write takes the last.
	_, err := s.Set(ctx, "k", "live", "v", tairhash.NoExpiry(), tairhash.Always)
	require.NoError(t, err)
	_, err = s.Set(ctx, "k", "live", "v", tairhash.NoExpiry(), tairhash.Always)
	require.NoError(t, err)

	n, err := s.vk.Do(ctx, s.vk.B().Hlen().Key(DataKey("k")).Build()).ToInt64()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	n, err = s.vk.Do(ctx, s.vk.B().Exists().Key(IndexKey("k")).Build()).ToInt64()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "index is removed once empty")
}

func TestSweep(t *testing.T) {
	s := newStore(t)
	ctx := testserver.Context(t)
	past := tairhash.ExpireAt(time.UnixMilli(1))

	const keys = 50
	for i := range keys {
		key := fmt.Sprintf("sweep:%d", i)
		_, err := s.Set(ctx, key, "dead", "v", past, tairhash.Always)
		require.NoError(t, err)
		_, err = s.Set(ctx, key, "live", "v", tairhash.TTL(time.Hour), tairhash.Always)
		require.NoError(t, err)
	}
	// The second write per key passively reclaimed its expired field already;
	// rewrite the dead fields without a following write so only Sweep can reclaim them.
	for i := range keys {
		_, err := s.Set(ctx, fmt.Sprintf("sweep:%d", i), "dead", "v", past, tairhash.Always)
		require.NoError(t, err)
	}

	removed, err := s.Sweep(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(10), removed, "budget bounds a single sweep")

	var total int64 = removed
	for range keys {
		n, err := s.Sweep(ctx, keys)
		require.NoError(t, err)
		total += n
		if total >= keys {
			break
		}
	}
	assert.Equal(t, int64(keys), total)

	for i := range keys {
		n, err := s.vk.Do(ctx, s.vk.B().Hlen().Key(DataKey(fmt.Sprintf("sweep:%d", i))).Build()).ToInt64()
		require.NoError(t, err)
		assert.Equal(t, int64(1), n)
	}
}

func TestReloadsLostLibrary(t *testing.T) {
	s := newStore(t)
	ctx := testserver.Context(t)
	require.NoError(t, s.vk.Do(ctx, s.vk.B().FunctionDelete().LibraryName("fieldhash").Build()).Error())

	ok, err := s.Set(ctx, "reload", "f", "v", tairhash.NoExpiry(), tairhash.Always)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestRejectsInexactIntegers(t *testing.T) {
	s := newStore(t)
	ctx := testserver.Context(t)
	_, err := s.SetWithArgs(ctx, "big", "f", "v", tairhash.SetArgs{Version: tairhash.ForceVersion(1 << 60)})
	assert.ErrorIs(t, err, tairhash.ErrInvalidArgument)
	_, err = s.IncrBy(ctx, "big", "c", 1<<60, tairhash.NoExpiry())
	assert.ErrorIs(t, err, tairhash.ErrInvalidArgument)
}

func TestDelBeyondUnpackLimit(t *testing.T) {
	s := newStore(t)
	ctx := testserver.Context(t)
	const n = 20_000
	fields := make([]string, n)
	for i := range fields {
		fields[i] = fmt.Sprintf("f%d", i)
	}
	for _, f := range fields[:n/2] {
		_, err := s.Set(ctx, "wide", f, "v", tairhash.TTL(time.Hour), tairhash.Always)
		require.NoError(t, err)
	}
	removed, err := s.Del(ctx, "wide", fields...)
	require.NoError(t, err)
	assert.Equal(t, int64(n/2), removed)
	all, err := s.GetAll(ctx, "wide")
	require.NoError(t, err)
	assert.Empty(t, all)
}
