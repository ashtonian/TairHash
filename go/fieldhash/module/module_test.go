package module

import (
	"testing"

	"github.com/ashtonian/TairHash/go/internal/conformance"
	"github.com/ashtonian/TairHash/go/internal/testserver"
	"github.com/ashtonian/TairHash/go/tairhash"
)

func TestConformance(t *testing.T) {
	s := New(tairhash.New(testserver.Client(t, testserver.DBModule)))
	t.Run("store", func(t *testing.T) { conformance.RunStore(t, s) })
	t.Run("versioned", func(t *testing.T) { conformance.RunVersioned(t, s) })
}
