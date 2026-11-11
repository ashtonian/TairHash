// Package testserver connects integration tests to a Valkey server.
//
// Tests skip unless VALKEY_ADDR is set. Each test package owns one database and
// flushes it per test, so packages can run in parallel against one server
// without touching db 0.
package testserver

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/valkey-io/valkey-go"
)

// Databases owned by each test package.
const (
	DBTairHash = 10 + iota
	DBModule
	DBLua
	DBNative
	DBPresence
	DBPlainHash
)

// Client returns a client on database db, flushed, and closes it at cleanup.
func Client(t testing.TB, db int) valkey.Client {
	t.Helper()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		t.Skip("VALKEY_ADDR not set; integration test skipped")
	}
	vk, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:      []string{addr},
		SelectDB:         db,
		DisableCache:     true,
		ConnWriteTimeout: 5 * time.Second,
	})
	require.NoError(t, err, "connect to %s", addr)
	t.Cleanup(vk.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, vk.Do(ctx, vk.B().Flushdb().Build()).Error(), "FLUSHDB %d", db)
	return vk
}

// Context returns a context bounded for a single integration test.
func Context(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
