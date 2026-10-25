package sim

import (
	"math"

	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/planner"

	"sort"
	"testing"
)

func TestDiagSeed3(t *testing.T) {
	s, r := setup(t)
	for _, name := range []string{"reference", "never pause"} {
		c := DefaultConfig(5)
		if name == "never pause" {
			c.Params.NoPauseH = math.Inf(1)
		}
		c.Trace = func(now float64, a planner.Action, ok bool) {
			t.Logf("t=%5.1f %s %s %s.%s in %s ok=%v (%s)", now, a.Kind, a.Workload, a.Family, a.Size, a.Zone, ok, a.Reason)
		}
		res := Run(s, r, c)
		ids := make([]string, 0)
		for id := range res.Jobs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		t.Logf("== %s: %.0f points", name, res.Score)
		for _, id := range ids {
			j := res.Jobs[id]
			w, _ := s.Workload(id)
			t.Logf("%-22s dl=%5.1f delivered=%v at %5.1f progress=%.2f co2=%.0f g", id, s.Hours(w.Deadline), j.Delivered, j.DeliveredAt, j.Progress, j.CarbonG)
		}
	}
}
