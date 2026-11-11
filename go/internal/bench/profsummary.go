package bench

import (
	"fmt"
	"io"
	"strings"

	"github.com/ashtonian/TairHash/go/internal/serverprof"
)

// WriteProfileSummary prints a server profile summary for a terminal.
func WriteProfileSummary(w io.Writer, s serverprof.Summary, out serverprof.Outputs) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: main thread %d samples, %d busy (%.1f%%), %d idle\n", s.Source, s.Samples, s.Busy, s.BusyPercent, s.Idle)
	section := func(title string, shares []serverprof.Share) {
		fmt.Fprintf(&b, "\n%s (%% of busy)\n", title)
		for _, sh := range shares {
			fmt.Fprintf(&b, "  %6.2f%%  %7d  %s\n", sh.Percent, sh.Samples, sh.Name)
		}
	}
	section("phases", s.Phases)
	section("components", s.Components)
	section("top self", s.TopSelf)
	section("top cumulative", s.TopCum)
	section("unclassified leaves", s.Unclassified)
	fmt.Fprintf(&b, "\nwrote %s, %s, %s, %s\n", out.MainPprof, out.AllPprof, out.Folded, out.Summary)
	_, err := io.WriteString(w, b.String())
	return err
}
