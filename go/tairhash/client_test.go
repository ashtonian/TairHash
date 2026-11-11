package tairhash

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashtonian/TairHash/go/internal/testserver"
)

// Behaviour shared with the other backends is covered by internal/conformance via
// fieldhash/module; these tests cover the commands only the driver exposes.

var past = ExpireAt(time.UnixMilli(1))

func newClient(t *testing.T) (*Client, context.Context) {
	t.Helper()
	c := New(testserver.Client(t, testserver.DBTairHash))
	ctx := testserver.Context(t)
	require.NoError(t, c.Ping(ctx))
	return c, ctx
}

func TestMultiFieldReads(t *testing.T) {
	c, ctx := newClient(t)
	require.NoError(t, c.MSet(ctx, "k", []FieldValue{{"a", "1"}, {"b", "22"}, {"c", "333"}}))
	_, err := c.Set(ctx, "k", "dead", "x", SetArgs{Expiry: past})
	require.NoError(t, err)

	got, err := c.MGet(ctx, "k", "c", "missing", "a", "dead")
	require.NoError(t, err)
	assert.Equal(t, []Field{{Value: "333", Found: true}, {}, {Value: "1", Found: true}, {}}, got)

	_, err = c.Set(ctx, "k", "a", "1b", SetArgs{})
	require.NoError(t, err)
	withVer, err := c.MGetWithVersion(ctx, "k", "a", "missing", "b")
	require.NoError(t, err)
	assert.Equal(t, []Field{{Value: "1b", Version: 2, Found: true}, {}, {Value: "22", Version: 1, Found: true}}, withVer)

	all, err := c.GetAllWithVersion(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, map[string]Entry{"a": {"1b", 2}, "b": {"22", 1}, "c": {"333", 1}}, all)

	keys, err := c.Keys(ctx, "k")
	require.NoError(t, err)
	sort.Strings(keys)
	assert.Equal(t, []string{"a", "b", "c"}, keys)

	vals, err := c.Vals(ctx, "k")
	require.NoError(t, err)
	sort.Strings(vals)
	assert.Equal(t, []string{"1b", "22", "333"}, vals)

	n, err := c.Len(ctx, "k", true)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)

	ok, err := c.Exists(ctx, "k", "b")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = c.Exists(ctx, "k", "dead")
	require.NoError(t, err)
	assert.False(t, ok)

	l, err := c.StrLen(ctx, "k", "c")
	require.NoError(t, err)
	assert.Equal(t, int64(3), l)
}

func TestScanVisitsEveryField(t *testing.T) {
	c, ctx := newClient(t)
	want := map[string]string{}
	fields := make([]FieldValue, 0, 100)
	for i := range 100 {
		f := FieldValue{Field: fmt.Sprintf("f%03d", i), Value: "v"}
		fields = append(fields, f)
		want[f.Field] = f.Value
	}
	require.NoError(t, c.MSet(ctx, "scan", fields))

	got := map[string]string{}
	var cursor uint64
	for {
		page, err := c.Scan(ctx, "scan", cursor, ScanArgs{Count: 7})
		require.NoError(t, err)
		for _, fv := range page.Fields {
			got[fv.Field] = fv.Value
		}
		if cursor = page.Cursor; cursor == 0 {
			break
		}
	}
	assert.Equal(t, want, got)

	page, err := c.Scan(ctx, "scan", 0, ScanArgs{Match: "f042", Count: 1000})
	require.NoError(t, err)
	assert.Equal(t, []FieldValue{{"f042", "v"}}, page.Fields)
}

func TestExpireAndPersist(t *testing.T) {
	c, ctx := newClient(t)
	_, err := c.Set(ctx, "e", "f", "v", SetArgs{})
	require.NoError(t, err)

	ok, err := c.Expire(ctx, "e", "f", TTL(time.Hour), VersionCheck{})
	require.NoError(t, err)
	assert.True(t, ok)
	ttl, err := c.PTTL(ctx, "e", "f")
	require.NoError(t, err)
	assert.True(t, ttl.Expires)

	_, err = c.Expire(ctx, "e", "f", TTL(time.Hour), MatchVersion(1))
	assert.ErrorIs(t, err, ErrStaleVersion, "expire bumped the version to 2")

	ok, err = c.Expire(ctx, "e", "f", ExpireAt(time.Now().Add(time.Hour)), MatchVersion(2))
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = c.Persist(ctx, "e", "f")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = c.Persist(ctx, "e", "f")
	require.NoError(t, err)
	assert.False(t, ok, "nothing to persist")

	ok, err = c.Expire(ctx, "e", "missing", TTL(time.Hour), VersionCheck{})
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = c.Expire(ctx, "e", "f", KeepTTL(), VersionCheck{})
	assert.ErrorIs(t, err, ErrInvalidArgument)
}

func TestVersionCommands(t *testing.T) {
	c, ctx := newClient(t)
	_, err := c.Set(ctx, "v", "f", "x", SetArgs{})
	require.NoError(t, err)

	ok, err := c.SetVersion(ctx, "v", "f", 40)
	require.NoError(t, err)
	assert.True(t, ok)
	ver, found, err := c.Version(ctx, "v", "f")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, int64(40), ver)

	_, found, err = c.Version(ctx, "v", "missing")
	require.NoError(t, err)
	assert.False(t, found)
	_, found, err = c.Version(ctx, "nokey", "f")
	require.NoError(t, err)
	assert.False(t, found)

	_, err = c.SetVersion(ctx, "v", "f", 0)
	assert.ErrorIs(t, err, ErrInvalidArgument)

	_, err = c.Set(ctx, "v", "g", "y", SetArgs{})
	require.NoError(t, err)
	n, err := c.DelWithVersion(ctx, "v", []FieldVersion{{"f", 39}, {"g", 1}})
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "only g matched")
	n, err = c.DelWithVersion(ctx, "v", []FieldVersion{{"f", 0}})
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "version 0 deletes unconditionally")
}

func TestSetNX(t *testing.T) {
	c, ctx := newClient(t)
	ok, err := c.SetNX(ctx, "nx", "f", "a")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = c.SetNX(ctx, "nx", "f", "b")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestMSetWithOptions(t *testing.T) {
	c, ctx := newClient(t)
	require.NoError(t, c.MSetWithOptions(ctx, "mo", []MSetEntry{
		{Field: "a", Value: "1", TTL: time.Hour},
		{Field: "b", Value: "2", TTL: 2 * time.Hour},
	}))
	got, err := c.GetAllWithVersion(ctx, "mo")
	require.NoError(t, err)
	assert.Equal(t, map[string]Entry{"a": {"1", 1}, "b": {"2", 1}}, got)

	err = c.MSetWithOptions(ctx, "mo", []MSetEntry{
		{Field: "a", Value: "new", Version: 1, TTL: time.Hour},
		{Field: "b", Value: "new", Version: 9, TTL: time.Hour},
	})
	assert.ErrorIs(t, err, ErrStaleVersion)
	v, _, err := c.Get(ctx, "mo", "a")
	require.NoError(t, err)
	assert.Equal(t, "1", v, "stale version anywhere aborts every write")

	for _, ttl := range []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond} {
		err = c.MSetWithOptions(ctx, "mo", []MSetEntry{{Field: "a", Value: "x", TTL: ttl}})
		assert.ErrorIs(t, err, ErrInvalidArgument, ttl)
	}
}

func TestIncrementBounds(t *testing.T) {
	c, ctx := newClient(t)
	limit := int64(10)
	n, err := c.IncrBy(ctx, "i", "c", 7, IncrArgs{Max: &limit})
	require.NoError(t, err)
	assert.Equal(t, int64(7), n)
	_, err = c.IncrBy(ctx, "i", "c", 4, IncrArgs{Max: &limit})
	assert.ErrorIs(t, err, ErrOverflow)

	f, err := c.IncrByFloat(ctx, "i", "f", 1.25, IncrFloatArgs{Expiry: TTL(time.Hour)})
	require.NoError(t, err)
	assert.InDelta(t, 1.25, f, 1e-9)
	f, err = c.IncrByFloat(ctx, "i", "f", -0.5, IncrFloatArgs{Expiry: KeepTTL()})
	require.NoError(t, err)
	assert.InDelta(t, 0.75, f, 1e-9)
	ttl, err := c.PTTL(ctx, "i", "f")
	require.NoError(t, err)
	assert.True(t, ttl.Expires)
}

func TestWrongType(t *testing.T) {
	c, ctx := newClient(t)
	require.NoError(t, c.vk.Do(ctx, c.vk.B().Set().Key("str").Value("x").Build()).Error())
	_, _, err := c.Get(ctx, "str", "f")
	assert.ErrorIs(t, err, ErrWrongType)
	_, err = c.Set(ctx, "str", "f", "v", SetArgs{})
	assert.ErrorIs(t, err, ErrWrongType)
}
