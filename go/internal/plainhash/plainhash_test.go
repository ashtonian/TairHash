package plainhash

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashtonian/TairHash/go/internal/testserver"
	"github.com/ashtonian/TairHash/go/tairhash"
)

func TestStore(t *testing.T) {
	s := New(testserver.Client(t, testserver.DBPlainHash))
	ctx := testserver.Context(t)

	ok, err := s.Set(ctx, "k", "a", "1", tairhash.NoExpiry(), tairhash.Always)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = s.Set(ctx, "k", "a", "2", tairhash.NoExpiry(), tairhash.IfNotExists)
	require.NoError(t, err)
	assert.False(t, ok, "HSETNX on an existing field")

	v, found, err := s.Get(ctx, "k", "a")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "1", v)

	n, err := s.IncrBy(ctx, "k", "a", 41, tairhash.KeepTTL())
	require.NoError(t, err)
	assert.Equal(t, int64(42), n)

	all, err := s.GetAll(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "42"}, all)

	ttl, err := s.PTTL(ctx, "k", "a")
	require.NoError(t, err)
	assert.Equal(t, tairhash.FieldTTL{Exists: true}, ttl)

	deleted, err := s.Del(ctx, "k", "a", "missing")
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
	_, found, err = s.Get(ctx, "k", "a")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestRejectsWhatAPlainHashCannotExpress(t *testing.T) {
	s := New(testserver.Client(t, testserver.DBPlainHash))
	ctx := testserver.Context(t)

	_, err := s.Set(ctx, "k", "f", "v", tairhash.TTL(time.Hour), tairhash.Always)
	assert.ErrorIs(t, err, ErrUnsupported)
	_, err = s.IncrBy(ctx, "k", "f", 1, tairhash.ExpireAt(time.Now().Add(time.Hour)))
	assert.ErrorIs(t, err, ErrUnsupported)
	_, err = s.Set(ctx, "k", "f", "v", tairhash.NoExpiry(), tairhash.IfExists)
	assert.ErrorIs(t, err, ErrUnsupported)
}
