package native

import (
	"testing"

	"github.com/ashtonian/TairHash/go/internal/conformance"
	"github.com/ashtonian/TairHash/go/internal/testserver"
)

func TestConformance(t *testing.T) {
	conformance.RunStore(t, New(testserver.Client(t, testserver.DBNative)))
}
