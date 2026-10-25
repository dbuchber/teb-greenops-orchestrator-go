package sim

import (
	"testing"

	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/spec"
)

func setup(t *testing.T) (*spec.Spec, Ref) {
	t.Helper()
	s, err := spec.Load("../../challenge")
	if err != nil {
		t.Fatal(err)
	}
	r, err := LoadRef("../../challenge")
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 15 {
		t.Fatalf("%d reference energies", len(r))
	}
	return s, r
}

func TestWorldDeterministic(t *testing.T) {
	a, b := NewWorld(7), NewWorld(7)
	if a.Actual["DE"][50] != b.Actual["DE"][50] || a.Forecast("AT", 30)[5] != b.Forecast("AT", 30)[5] {
		t.Fatal("same seed, different world")
	}
	if NewWorld(8).Actual["DE"][50] == a.Actual["DE"][50] {
		t.Fatal("different seed, same world")
	}
}

func TestRunDeterministicAndLegal(t *testing.T) {
	s, r := setup(t)
	a := Run(s, r, DefaultConfig(3))
	b := Run(s, r, DefaultConfig(3))
	if a.Score != b.Score {
		t.Fatalf("not deterministic: %.2f vs %.2f", a.Score, b.Score)
	}
	if a.Disqualified {
		t.Fatal("disqualified")
	}
	if a.Score <= 0 || a.Score > 25000 {
		t.Fatalf("points %.1f", a.Score)
	}
	t.Logf("seed 3: %.1f points, %d/15 on time, %.2f kg", a.Score, a.OnTime, a.ElectricityG/1000)
}

// Global planning must complete a full run legally, deterministically and fully on time.
func TestGlobalRunOnTime(t *testing.T) {
	s, r := setup(t)
	cfg := func() Config {
		c := DefaultConfig(3)
		c.Params.Global = true
		return c
	}
	a, b := Run(s, r, cfg()), Run(s, r, cfg())
	if a.Score != b.Score {
		t.Fatalf("not deterministic: %.2f vs %.2f", a.Score, b.Score)
	}
	if a.Disqualified || a.OnTime != 15 {
		t.Fatalf("disqualified %v, on time %d/15", a.Disqualified, a.OnTime)
	}
	t.Logf("seed 3 global: %.1f points, %.2f kg", a.Score, a.ElectricityG/1000)
}
