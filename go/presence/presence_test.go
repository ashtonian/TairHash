package presence

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ashtonian/TairHash/go/fieldhash"
	"github.com/ashtonian/TairHash/go/fieldhash/lua"
	"github.com/ashtonian/TairHash/go/fieldhash/module"
	"github.com/ashtonian/TairHash/go/fieldhash/native"
	"github.com/ashtonian/TairHash/go/internal/testserver"
	"github.com/ashtonian/TairHash/go/tairhash"
)

func backends(t *testing.T) map[string]fieldhash.Store {
	vk := testserver.Client(t, testserver.DBPresence)
	l, err := lua.New(testserver.Context(t), vk)
	require.NoError(t, err)
	return map[string]fieldhash.Store{
		"module": module.New(tairhash.New(vk)),
		"lua":    l,
		"native": native.New(vk),
	}
}

func TestRegistry(t *testing.T) {
	for name, store := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := testserver.Context(t)
			r, err := New(store, WithKeyPrefix("presence:"+name+":"), WithTTL(time.Hour))
			require.NoError(t, err)

			require.NoError(t, r.Heartbeat(ctx, "u1", Session{Device: "phone", Node: "gw-1", Status: "active"}))
			require.NoError(t, r.Heartbeat(ctx, "u1", Session{Device: "laptop", Node: "gw-2"}))
			require.NoError(t, r.Heartbeat(ctx, "u1", Session{Device: "phone", Node: "gw-3", Status: "idle"}))

			online, err := r.Online(ctx, "u1")
			require.NoError(t, err)
			assert.Equal(t, []Session{
				{Device: "laptop", Node: "gw-2"},
				{Device: "phone", Node: "gw-3", Status: "idle"},
			}, online)

			s, found, err := r.Lookup(ctx, "u1", "phone")
			require.NoError(t, err)
			assert.True(t, found)
			assert.Equal(t, "gw-3", s.Node)

			ok, err := r.Disconnect(ctx, "u1", "laptop")
			require.NoError(t, err)
			assert.True(t, ok)
			ok, err = r.Disconnect(ctx, "u1", "laptop")
			require.NoError(t, err)
			assert.False(t, ok)

			// A device that stopped heartbeating: its lease is already in the past.
			_, err = store.Set(ctx, r.key("u1"), "tablet", `{"node":"gw-9"}`, tairhash.ExpireAt(time.UnixMilli(1)), tairhash.Always)
			require.NoError(t, err)
			online, err = r.Online(ctx, "u1")
			require.NoError(t, err)
			assert.Equal(t, []Session{{Device: "phone", Node: "gw-3", Status: "idle"}}, online)

			online, err = r.Online(ctx, "nobody")
			require.NoError(t, err)
			assert.Empty(t, online)
		})
	}
}

func TestValidation(t *testing.T) {
	_, err := New(nil, WithTTL(0))
	assert.Error(t, err)

	r, err := New(nil)
	require.NoError(t, err)
	assert.Equal(t, DefaultTTL, r.TTL())
	assert.ErrorIs(t, r.Heartbeat(t.Context(), "", Session{Device: "d"}), ErrInvalidSession)
	assert.ErrorIs(t, r.Heartbeat(t.Context(), "u", Session{}), ErrInvalidSession)
}
