// Package planner entscheidet je Takt, welche VMs angelegt, gestartet, gestoppt oder gelöscht werden.
// Plan ist rein (keine Uhr, kein Netzwerk): gleicher Input, gleiche Aktionen. Das macht ihn testbar und schnell.
//
// Grundgerüst-Strategie (bewusst einfach, zum Ausbauen):
//   - Wo: legale (Zone, Profil)-Option mit den geringsten erwarteten Emissionen
//     = Wirkleistung × PUE × Rechenstunden × Mittel der günstigsten Prognosestunden.
//   - Wann: eine VM rechnet in den günstigsten k Stunden des Fensters (k = nötige Stunden) und wird in den
//     anderen gestoppt (0 W, Fortschritt bleibt). Bei wenig Spielraum läuft sie durch.
//   - Aufräumen: VMs löschen, sobald final_output_delivered gilt.
//
// Offen (TODO): mehrere Worker je Job, Kontingent-Reservierung für spätere Jobs, Parken (Löschen und
// Neuanlegen) flexibler Jobs, Vorab-Anlegen vor dem Release, Prognosekorrektur aus eigenen Beobachtungen.
package planner

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/spec"
)

type Kind string

const (
	Create Kind = "create"
	Start  Kind = "start"
	Stop   Kind = "stop"
	Delete Kind = "delete"
)

type Action struct {
	Kind     Kind
	Workload string
	Zone     string
	Family   string
	Size     string
	VMID     string
	Name     string
	Reason   string
}

// VM ist eine eigene VM mit dem Profil, das ihr Name kodiert.
type VM struct {
	ID, Name, Zone, Family, Size string
	State                        string // creating, running, stopped
}

type Job struct {
	Completed float64
	Delivered bool
	VMs       []VM
}

type Input struct {
	Now      float64              // Stunden seit T0 (aus virtual_time)
	Jobs     map[string]Job       // je Workload-ID
	Forecast map[string][]float64 // je Strombereich: g/kWh ab der aktuellen Stunde, Index 0 = jetzt
}

type Params struct {
	MarginH     float64 // Puffer vor der Deadline (h)
	NoPauseH    float64 // Spielraum, unter dem nicht mehr pausiert wird (h)
	FallbackGCO float64 // Annahme für Stunden ohne Prognose (g/kWh)
}

func DefaultParams() Params { return Params{MarginH: 2, NoPauseH: 2, FallbackGCO: 250} }

// Name kodiert Workload und Profil, damit ein Neustart des Orchestrators eigene VMs wiedererkennt.
func Name(workload, family, size string) string {
	return "gx-" + workload + "--" + family + "--" + size
}

// ParseName ist die Umkehrung von Name; ok ist falsch für fremde VMs.
func ParseName(name string) (workload, family, size string, ok bool) {
	rest, found := strings.CutPrefix(name, "gx-")
	if !found {
		return "", "", "", false
	}
	p := strings.Split(rest, "--")
	if len(p) != 3 {
		return "", "", "", false
	}
	return p[0], p[1], p[2], true
}

type usage struct {
	vms, vcpus int
	gpus       map[string]int
}

func (u *usage) add(p spec.Profile) {
	u.vms++
	u.vcpus += p.CPUCores
	if p.GPUCount > 0 {
		u.gpus[p.Family] += p.GPUCount
	}
}

func (u *usage) fits(s *spec.Spec, p spec.Profile) bool {
	if u.vms+1 > s.MaxVMs || u.vcpus+p.CPUCores > s.MaxVCPUs {
		return false
	}
	return p.GPUCount == 0 || u.gpus[p.Family]+p.GPUCount <= s.GPUQuota[p.Family]
}

// cheapest ist das Mittel der k günstigsten Prognosestunden in fc[:window]. Braucht der Job mehr Stunden, als
// die Prognose reicht, zählen die übrigen mit dem Prognosemittel; ein fester Ersatzwert würde lange Laufzeiten
// (sparsame, langsame Profile) systematisch benachteiligen. fallback nur ohne jede Prognose.
func cheapest(fc []float64, window, k int, fallback float64) float64 {
	n := min(window, len(fc))
	if n <= 0 {
		return fallback
	}
	vals := slices.Clone(fc[:n])
	sort.Float64s(vals)
	k = max(k, 1)
	sum, all := 0.0, 0.0
	for i, v := range vals {
		all += v
		if i < k {
			sum += v
		}
	}
	if k > n {
		sum += float64(k-n) * all / float64(n)
	}
	return sum / float64(k)
}

func Plan(s *spec.Spec, in Input, p Params) []Action {
	var out []Action
	u := usage{gpus: map[string]int{}}
	for _, j := range in.Jobs {
		for _, vm := range j.VMs {
			if prof, ok := s.Profile(vm.Family, vm.Size); ok {
				u.add(prof)
			}
		}
	}
	// Engste Deadline zuerst: sie bekommen Kontingent vor den flexiblen Jobs.
	wls := slices.Clone(s.Workloads)
	sort.SliceStable(wls, func(a, b int) bool { return wls[a].Deadline.Before(wls[b].Deadline) })

	for _, w := range wls {
		j := in.Jobs[w.ID]
		if j.Delivered {
			for _, vm := range j.VMs {
				out = append(out, Action{Kind: Delete, Workload: w.ID, Zone: vm.Zone, VMID: vm.ID, Name: vm.Name, Reason: "geliefert"})
			}
			continue
		}
		release, deadline := s.Hours(w.Release), s.Hours(w.Deadline)
		remaining := math.Max(w.RequiredWork-j.Completed, 0)

		if len(j.VMs) == 0 {
			if a, ok := placeNew(s, in, p, w, &u, release, deadline, remaining); ok {
				out = append(out, a)
			}
			continue
		}
		vm := j.VMs[0] // Grundgerüst: ein Worker je Job
		if vm.State == "creating" || vm.ID == "" {
			continue
		}
		prof, ok := s.Profile(vm.Family, vm.Size)
		if !ok {
			continue
		}
		run, why := shouldRun(s, in, p, w, prof, vm.Zone, release, deadline, remaining)
		switch {
		case run && vm.State == "stopped":
			out = append(out, Action{Kind: Start, Workload: w.ID, Zone: vm.Zone, VMID: vm.ID, Name: vm.Name, Reason: why})
		case !run && vm.State == "running":
			out = append(out, Action{Kind: Stop, Workload: w.ID, Zone: vm.Zone, VMID: vm.ID, Name: vm.Name, Reason: why})
		}
	}
	return out
}

// placeNew wählt für einen Job ohne VM die legale Option mit den geringsten erwarteten Emissionen.
func placeNew(s *spec.Spec, in Input, p Params, w spec.Workload, u *usage, release, deadline, remaining float64) (Action, bool) {
	if in.Now < release-1 { // Grundgerüst: frühestens eine Stunde vor dem Release anlegen
		return Action{}, false
	}
	start := math.Max(in.Now, release)
	best, bestCost := spec.Option{}, math.Inf(1)
	for _, o := range s.Options(w) {
		compute := spec.ComputeHours(w, o.Profile, 1, remaining)
		setup := float64(o.Profile.BootS+w.InputS) / 3600
		total := setup + compute + float64(w.OutputS)/3600
		if start+total+p.MarginH > deadline || !u.fits(s, o.Profile) {
			continue
		}
		window := int(math.Floor(deadline - p.MarginH - start - setup))
		g := cheapest(in.Forecast[o.Zone.Area], window, spec.Ceil(compute), p.FallbackGCO)
		cost := o.Profile.ActiveW / 1000 * o.Zone.PUE * compute * g
		if cost < bestCost {
			best, bestCost = o, cost
		}
	}
	if math.IsInf(bestCost, 1) {
		return Action{}, false
	}
	u.add(best.Profile)
	return Action{Kind: Create, Workload: w.ID, Zone: best.Zone.Slug, Family: best.Profile.Family, Size: best.Profile.Size,
		Name:   Name(w.ID, best.Profile.Family, best.Profile.Size),
		Reason: fmt.Sprintf("günstigste Option, erwartet %.0f g", bestCost)}, true
}

// shouldRun: läuft die VM jetzt? Ja, wenn der Spielraum knapp ist oder die aktuelle Stunde zu den
// günstigsten k Stunden bis zur Deadline (innerhalb der Prognose) gehört.
func shouldRun(s *spec.Spec, in Input, p Params, w spec.Workload, prof spec.Profile, zone string, release, deadline, remaining float64) (bool, string) {
	if in.Now < release {
		return true, "vor Release: Input laden"
	}
	compute := spec.ComputeHours(w, prof, 1, remaining)
	need := compute + float64(w.OutputS)/3600
	slack := deadline - p.MarginH - in.Now - need
	if slack < p.NoPauseH || compute <= 0 {
		return true, fmt.Sprintf("Spielraum %.1f h", slack)
	}
	fc := in.Forecast[s.Zones[zone].Area]
	if len(fc) == 0 {
		return true, "keine Prognose"
	}
	window := min(len(fc), int(math.Floor(deadline-p.MarginH-in.Now)))
	k := spec.Ceil(compute)
	if k >= window {
		return true, "braucht das ganze Prognosefenster"
	}
	vals := slices.Clone(fc[:window])
	sort.Float64s(vals)
	limit := vals[k-1]
	if fc[0] <= limit {
		return true, fmt.Sprintf("jetzt %.0f ≤ Schwelle %.0f g/kWh", fc[0], limit)
	}
	return false, fmt.Sprintf("jetzt %.0f > Schwelle %.0f g/kWh, Spielraum %.1f h", fc[0], limit, slack)
}
