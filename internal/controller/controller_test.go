package controller

import (
	"testing"
	"time"

	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/api"
	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/planner"
	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/spec"
)

// The clock VM is bound to a workload: its Create must be legal, otherwise the attempt alone disqualifies.
func TestClockIsBoundAndLegal(t *testing.T) {
	s, err := spec.Load("../../challenge")
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Spec: s, vms: map[string]planner.VM{}, pending: map[string]time.Time{}}
	acts := c.clock(api.Status{State: "READY"})
	if len(acts) != 1 || acts[0].Kind != planner.Create || acts[0].Workload == "" || !isClock(acts[0].Name) {
		t.Fatalf("%+v", acts)
	}
	a := acts[0]
	w, _ := s.Workload(a.Workload)
	p, _ := s.Profile(a.Family, a.Size)
	if err := s.Legal(w, a.Zone, p); err != nil {
		t.Fatalf("clock VM illegal: %v", err)
	}
	for _, x := range s.Workloads {
		if x.Release.After(w.Release) {
			t.Fatalf("%s is released before %s", w.ID, x.ID)
		}
	}
}
