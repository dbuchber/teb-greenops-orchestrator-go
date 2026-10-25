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
		t.Fatalf("%d Referenzenergien", len(r))
	}
	return s, r
}

func TestWorldDeterministic(t *testing.T) {
	a, b := NewWorld(7), NewWorld(7)
	if a.Actual["DE"][50] != b.Actual["DE"][50] || a.Forecast("AT", 30)[5] != b.Forecast("AT", 30)[5] {
		t.Fatal("gleicher Seed, andere Welt")
	}
	if NewWorld(8).Actual["DE"][50] == a.Actual["DE"][50] {
		t.Fatal("anderer Seed, gleiche Welt")
	}
}

func TestRunDeterministicAndLegal(t *testing.T) {
	s, r := setup(t)
	a := Run(s, r, DefaultConfig(3))
	b := Run(s, r, DefaultConfig(3))
	if a.Score != b.Score {
		t.Fatalf("nicht deterministisch: %.2f vs %.2f", a.Score, b.Score)
	}
	if a.Disqualified {
		t.Fatal("disqualifiziert")
	}
	if a.Score <= 0 || a.Score > 25000 {
		t.Fatalf("Punkte %.1f", a.Score)
	}
	t.Logf("Seed 3: %.1f Punkte, %d/15 pünktlich, %.2f kg", a.Score, a.OnTime, a.ElectricityG/1000)
}

// Die globale Planung muss einen vollen Lauf legal, deterministisch und vollständig pünktlich schaffen.
func TestGlobalRunOnTime(t *testing.T) {
	s, r := setup(t)
	cfg := func() Config {
		c := DefaultConfig(3)
		c.Params.Global = true
		return c
	}
	a, b := Run(s, r, cfg()), Run(s, r, cfg())
	if a.Score != b.Score {
		t.Fatalf("nicht deterministisch: %.2f vs %.2f", a.Score, b.Score)
	}
	if a.Disqualified || a.OnTime != 15 {
		t.Fatalf("disqualifiziert %v, pünktlich %d/15", a.Disqualified, a.OnTime)
	}
	t.Logf("Seed 3 global: %.1f Punkte, %.2f kg", a.Score, a.ElectricityG/1000)
}
