package spec

import (
	"math"
	"testing"
)

func load(t *testing.T) *Spec {
	t.Helper()
	s, err := Load("../../challenge")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCatalog(t *testing.T) {
	s := load(t)
	if len(s.Zones) != 11 || len(s.Profiles) != 22 || len(s.Workloads) != 15 {
		t.Fatalf("zones %d profiles %d workloads %d", len(s.Zones), len(s.Profiles), len(s.Workloads))
	}
	if s.MaxVMs != 20 || s.MaxVCPUs != 320 || s.GPUQuota["gpurtx6000pro"] != 4 {
		t.Fatalf("quota %d %d %v", s.MaxVMs, s.MaxVCPUs, s.GPUQuota)
	}
	if got := s.T0.Format("2006-01-02T15"); got != "2025-09-30T00" {
		t.Fatalf("T0 %s", got)
	}
}

// Every option must be legal, and no option may lead to a forbidden country.
func TestOptionsRespectResidency(t *testing.T) {
	s := load(t)
	for _, w := range s.Workloads {
		opts := s.Options(w)
		if len(opts) == 0 {
			t.Fatalf("%s: no legal option", w.ID)
		}
		for _, o := range opts {
			if w.Residency == "EU" && o.Zone.Country == "CH" {
				t.Fatalf("%s: CH with EU residency", w.ID)
			}
			for _, f := range w.Forbidden {
				if o.Zone.Country == f {
					t.Fatalf("%s: forbidden country %s", w.ID, f)
				}
			}
		}
	}
}

func TestLegalExamples(t *testing.T) {
	s := load(t)
	w02, _ := s.Workload("w-02-ch-reporting")
	mega, _ := s.Profile("cpu", "mega")
	if s.Legal(w02, "ch-gva-2", mega) != nil {
		t.Fatal("w-02 must be allowed to run in Geneva")
	}
	if s.Legal(w02, "de-fra-1", mega) == nil {
		t.Fatal("w-02 must not go to Frankfurt")
	}
	w09, _ := s.Workload("w-09-eu-large-model")
	a40, _ := s.Profile("gpu3", "small")
	if s.Legal(w09, "de-fra-1", a40) == nil {
		t.Fatal("w-09 needs 96 GiB, A40 has 48")
	}
	rtx, _ := s.Profile("gpurtx6000pro", "small")
	if s.Legal(w09, "ch-dk-2", rtx) == nil {
		t.Fatal("w-09 is EU, Zurich is not")
	}
	if s.Legal(w09, "hr-zag-1", rtx) != nil {
		t.Fatal("w-09 in Zagreb on RTX PRO 6000 is legal")
	}
}

func TestComputeHours(t *testing.T) {
	s := load(t)
	w09, _ := s.Workload("w-09-eu-large-model")
	rtx, _ := s.Profile("gpurtx6000pro", "small")
	if h := ComputeHours(w09, rtx, 1, w09.RequiredWork); math.Abs(h-43) > 0.1 {
		t.Fatalf("w-09 on RTX PRO 6000 x1: %.2f h, expected 43", h)
	}
	w01, _ := s.Workload("w-01-dach-research")
	mega, _ := s.Profile("cpu", "mega")
	if h := ComputeHours(w01, mega, 1, w01.RequiredWork); math.Abs(h-31.25) > 0.01 {
		t.Fatalf("w-01 on mega: %.2f h", h)
	}
}
