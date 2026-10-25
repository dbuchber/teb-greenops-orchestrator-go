package planner

import (
	"testing"

	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/spec"
)

func load(t *testing.T) *spec.Spec {
	t.Helper()
	s, err := spec.Load("../../challenge")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// flat liefert für alle Bereiche eine konstante Prognose, ausgewählte Bereiche billiger.
func flat(s *spec.Spec, g float64, cheap map[string]float64) map[string][]float64 {
	out := map[string][]float64{}
	for _, z := range s.Zones {
		v := g
		if c, ok := cheap[z.Area]; ok {
			v = c
		}
		fc := make([]float64, 24)
		for i := range fc {
			fc[i] = v
		}
		out[z.Area] = fc
	}
	return out
}

func TestNameRoundTrip(t *testing.T) {
	w, f, sz, ok := ParseName(Name("w-07-eu-training", "gpurtx6000pro", "small"))
	if !ok || w != "w-07-eu-training" || f != "gpurtx6000pro" || sz != "small" {
		t.Fatal(w, f, sz, ok)
	}
	if _, _, _, ok := ParseName("fremde-vm"); ok {
		t.Fatal("fremde VM erkannt")
	}
}

// Jedes Create muss legal sein, und das Kontingent darf nie überschritten werden.
func TestCreatesAreLegalAndFitQuota(t *testing.T) {
	s := load(t)
	in := Input{Now: 50, Jobs: map[string]Job{}, Forecast: flat(s, 200, nil)}
	acts := Plan(s, in, DefaultParams())
	vms, vcpus, gpus := 0, 0, map[string]int{}
	for _, a := range acts {
		if a.Kind != Create {
			t.Fatalf("unerwartete Aktion %+v", a)
		}
		w, _ := s.Workload(a.Workload)
		p, _ := s.Profile(a.Family, a.Size)
		if err := s.Legal(w, a.Zone, p); err != nil {
			t.Fatalf("illegal: %v", err)
		}
		vms++
		vcpus += p.CPUCores
		gpus[p.Family] += p.GPUCount
	}
	if vms == 0 || vms > s.MaxVMs || vcpus > s.MaxVCPUs {
		t.Fatalf("vms %d vcpus %d", vms, vcpus)
	}
	for f, n := range gpus {
		if f != "cpu" && n > s.GPUQuota[f] {
			t.Fatalf("GPU %s: %d > %d", f, n, s.GPUQuota[f])
		}
	}
}

func TestPrefersCleanCountry(t *testing.T) {
	s := load(t)
	in := Input{Now: 4, Jobs: map[string]Job{}, Forecast: flat(s, 400, map[string]float64{"AT": 50})}
	for _, a := range Plan(s, in, DefaultParams()) {
		if a.Workload == "w-01-dach-research" && s.Zones[a.Zone].Country != "AT" {
			t.Fatalf("w-01 nach %s statt AT", a.Zone)
		}
	}
}

func TestDeletesDelivered(t *testing.T) {
	s := load(t)
	vm := VM{ID: "v1", Name: Name("w-04-dach-batch", "cpu", "mega"), Zone: "at-vie-1", Family: "cpu", Size: "mega", State: "running"}
	in := Input{Now: 60, Jobs: map[string]Job{"w-04-dach-batch": {Delivered: true, VMs: []VM{vm}}}, Forecast: flat(s, 200, nil)}
	for _, a := range Plan(s, in, DefaultParams()) {
		if a.Workload == "w-04-dach-batch" {
			if a.Kind != Delete || a.VMID != "v1" {
				t.Fatalf("%+v", a)
			}
			return
		}
	}
	t.Fatal("keine Löschung")
}

func TestPausesInDirtyHourWithSlack(t *testing.T) {
	s := load(t)
	fc := flat(s, 100, nil)
	fc["AT"][0] = 500 // jetzt teuer, später billig
	vm := VM{ID: "v1", Name: Name("w-13-regional-index", "cpu", "mega"), Zone: "at-vie-1", Family: "cpu", Size: "mega", State: "running"}
	in := Input{Now: 20, Jobs: map[string]Job{"w-13-regional-index": {Completed: 1300, VMs: []VM{vm}}}, Forecast: fc}
	for _, a := range Plan(s, in, DefaultParams()) {
		if a.Workload == "w-13-regional-index" && a.Kind == Stop {
			return
		}
	}
	t.Fatal("keine Pause trotz Spielraum")
}

func TestRunsWhenNoSlack(t *testing.T) {
	s := load(t)
	fc := flat(s, 100, nil)
	fc["AT"][0] = 500
	vm := VM{ID: "v1", Name: Name("w-01-dach-research", "cpu", "mega"), Zone: "at-vie-1", Family: "cpu", Size: "mega", State: "stopped"}
	// w-01: Deadline 108 h, 1 000 vCPU-h auf mega = 31 h; bei t = 74 h bleibt kein Spielraum
	in := Input{Now: 74, Jobs: map[string]Job{"w-01-dach-research": {Completed: 0, VMs: []VM{vm}}}, Forecast: fc}
	for _, a := range Plan(s, in, DefaultParams()) {
		if a.Workload == "w-01-dach-research" && a.Kind == Start {
			return
		}
	}
	t.Fatal("kein Start trotz knapper Deadline")
}
