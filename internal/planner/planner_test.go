package planner

import (
	"fmt"
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

// legacy are the parameters of the old local planner; its tests check it explicitly.
func legacy() Params {
	p := DefaultParams()
	p.Global = false
	return p
}

// flat returns a constant forecast for all areas, selected areas cheaper.
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
	if _, _, _, ok := ParseName("foreign-vm"); ok {
		t.Fatal("foreign VM recognised")
	}
}

// Every Create must be legal, and the quota must never be exceeded.
func TestCreatesAreLegalAndFitQuota(t *testing.T) {
	s := load(t)
	in := Input{Now: 50, Jobs: map[string]Job{}, Forecast: flat(s, 200, nil)}
	acts := Plan(s, in, legacy())
	vms, vcpus, gpus := 0, 0, map[string]int{}
	for _, a := range acts {
		if a.Kind != Create {
			t.Fatalf("unexpected action %+v", a)
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
	for _, a := range Plan(s, in, legacy()) {
		if a.Workload == "w-01-dach-research" && s.Zones[a.Zone].Country != "AT" {
			t.Fatalf("w-01 to %s instead of AT", a.Zone)
		}
	}
}

func TestDeletesDelivered(t *testing.T) {
	s := load(t)
	vm := VM{ID: "v1", Name: Name("w-04-dach-batch", "cpu", "mega"), Zone: "at-vie-1", Family: "cpu", Size: "mega", State: "running"}
	in := Input{Now: 60, Jobs: map[string]Job{"w-04-dach-batch": {Delivered: true, VMs: []VM{vm}}}, Forecast: flat(s, 200, nil)}
	for _, a := range Plan(s, in, legacy()) {
		if a.Workload == "w-04-dach-batch" {
			if a.Kind != Delete || a.VMID != "v1" {
				t.Fatalf("%+v", a)
			}
			return
		}
	}
	t.Fatal("no deletion")
}

func TestPausesInDirtyHourWithSlack(t *testing.T) {
	s := load(t)
	fc := flat(s, 100, nil)
	fc["AT"][0] = 500 // expensive now, cheap later
	vm := VM{ID: "v1", Name: Name("w-13-regional-index", "cpu", "mega"), Zone: "at-vie-1", Family: "cpu", Size: "mega", State: "running"}
	in := Input{Now: 20, Jobs: map[string]Job{"w-13-regional-index": {Completed: 1300, VMs: []VM{vm}}}, Forecast: fc}
	for _, a := range Plan(s, in, legacy()) {
		if a.Workload == "w-13-regional-index" && a.Kind == Stop {
			return
		}
	}
	t.Fatal("no pause despite slack")
}

func TestRunsWhenNoSlack(t *testing.T) {
	s := load(t)
	fc := flat(s, 100, nil)
	fc["AT"][0] = 500
	vm := VM{ID: "v1", Name: Name("w-01-dach-research", "cpu", "mega"), Zone: "at-vie-1", Family: "cpu", Size: "mega", State: "stopped"}
	// w-01: deadline 108 h, 1,000 vCPU-h on mega = 31 h; at t = 74 h there is no slack left
	in := Input{Now: 74, Jobs: map[string]Job{"w-01-dach-research": {Completed: 0, VMs: []VM{vm}}}, Forecast: fc}
	for _, a := range Plan(s, in, legacy()) {
		if a.Workload == "w-01-dach-research" && a.Kind == Start {
			return
		}
	}
	t.Fatal("no start despite tight deadline")
}

// Stopped VMs block the quota: if a job waits without room, VMs are started instead of paused.
func TestQuotaPressureResumesStoppedVMs(t *testing.T) {
	s := load(t)
	fc := flat(s, 100, nil)
	for a := range fc {
		fc[a][0] = 900 // very expensive now: without pressure everything would pause
	}
	jobs := map[string]Job{}
	ids := []string{"w-01-dach-research", "w-02-ch-reporting", "w-03-med-analysis", "w-04-dach-batch", "w-05-eu-render",
		"w-06-large-etl", "w-11-dach-simulation", "w-12-late-delivery", "w-13-regional-index", "w-14-bulk-render"}
	for i, id := range ids { // 10 × mega = 320 vCPU, all stopped
		zone := "de-fra-1"
		if id == "w-02-ch-reporting" {
			zone = "ch-gva-2"
		} else if id == "w-03-med-analysis" || id == "w-14-bulk-render" {
			zone = "it-mil-1"
		}
		jobs[id] = Job{Completed: 100, VMs: []VM{{ID: "v" + string(rune('a'+i)), Name: Name(id, "cpu", "mega"), Zone: zone,
			Family: "cpu", Size: "mega", State: "stopped"}}}
	}
	in := Input{Now: 50, Jobs: jobs, Forecast: fc} // w-15 (release 30) waits without room
	starts := 0
	for _, a := range Plan(s, in, legacy()) {
		if a.Kind == Stop {
			t.Fatalf("pause despite quota pressure: %+v", a)
		}
		if a.Kind == Start {
			starts++
		}
	}
	if starts != len(ids) {
		t.Fatalf("%d of %d stopped VMs started", starts, len(ids))
	}
}

// Global planning: every Create legal, the quota never exceeded; a full run in the simulator is
// covered in sim_test.
func TestGlobalCreatesAreLegalAndFitQuota(t *testing.T) {
	s := load(t)
	p := DefaultParams()
	p.Global, p.Cache = true, &GlobalCache{}
	for _, now := range []float64{0, 20, 50, 80} {
		p.Cache = &GlobalCache{}
		in := Input{Now: now, Jobs: map[string]Job{}, Forecast: flat(s, 200, map[string]float64{"CH": 40})}
		vms, vcpus, gpus := 0, 0, map[string]int{}
		for _, a := range Plan(s, in, p) {
			if a.Kind != Create {
				t.Fatalf("unexpected action %+v", a)
			}
			w, _ := s.Workload(a.Workload)
			pr, _ := s.Profile(a.Family, a.Size)
			if err := s.Legal(w, a.Zone, pr); err != nil {
				t.Fatalf("t=%.0f illegal: %v", now, err)
			}
			vms++
			vcpus += pr.CPUCores
			gpus[pr.Family] += pr.GPUCount
		}
		if vms > s.MaxVMs || vcpus > s.MaxVCPUs {
			t.Fatalf("t=%.0f: vms %d vcpus %d", now, vms, vcpus)
		}
		for f, n := range gpus {
			if f != "cpu" && n > s.GPUQuota[f] {
				t.Fatalf("t=%.0f GPU %s: %d > %d", now, f, n, s.GPUQuota[f])
			}
		}
	}
}

// Compute time of one global plan at the start (all jobs open): go test ./internal/planner -bench Global
func BenchmarkGlobalPlan(b *testing.B) {
	s, err := spec.Load("../../challenge")
	if err != nil {
		b.Fatal(err)
	}
	for _, orders := range []int{1, 12, 24, 48} {
		b.Run(fmt.Sprintf("orders=%d", orders), func(b *testing.B) {
			p := DefaultParams()
			p.AllEnds, p.Orders = true, orders
			in := Input{Now: 10, Jobs: map[string]Job{}, Forecast: flat(s, 200, map[string]float64{"CH": 40})}
			for i := 0; i < b.N; i++ {
				p.Cache = &GlobalCache{}
				Plan(s, in, p)
			}
		})
	}
}
