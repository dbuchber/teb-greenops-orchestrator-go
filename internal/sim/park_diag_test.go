package sim

import (
	"os"
	"strings"
	"testing"

	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/planner"
)

// Diagnose Parken: PARK_DIAG=1 go test ./internal/sim -run ParkDiag -v
func TestParkDiag(t *testing.T) {
	if os.Getenv("PARK_DIAG") == "" {
		t.Skip("nur auf Anfrage")
	}
	s, r := setup(t)
	for _, park := range []float64{0, 6} {
		c := DefaultConfig(7)
		c.Params.ParkGapH, c.Params.ParkLossH = park, 0.1
		c.Trace = func(now float64, a planner.Action, ok bool) {
			if a.Kind == planner.Create || strings.HasPrefix(a.Reason, "parken") {
				t.Logf("park=%g t=%5.1f %-6s %-20s %s.%s %s (%s)", park, now, a.Kind, a.Workload, a.Family, a.Size, a.Zone, a.Reason)
			}
		}
		res := Run(s, r, c)
		t.Logf("park=%g: %.0f Punkte, %.2f kg", park, res.Score, res.ElectricityG/1000)
		for id, j := range res.Jobs {
			t.Logf("  %s %.0f g", id, j.CarbonG)
		}
	}
}
