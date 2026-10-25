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
	ReserveH    float64 // Jobs, die spätestens in so vielen Stunden starten müssen, reservieren ihr Kontingent
	QuotaCost   float64 // Aufschlag auf die Kosten je Anteil am vCPU-Kontingent (0,5: 144 von 320 vCPU = +22 %)
}

func DefaultParams() Params {
	// ReserveH aus: mit QuotaCost liefert die Reservierung im Simulator weniger (cmd/sim, 100 Seeds)
	return Params{MarginH: 2, NoPauseH: 2, FallbackGCO: 250, ReserveH: -1, QuotaCost: 1}
}

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
	pressure := quotaPressure(s, in, wls, u)
	reserve(s, in, p, wls, &u)

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
		if !run && pressure != "" {
			// gestoppte VMs behalten ihr Kontingent: solange ein freigegebener Job keinen Platz findet, wird
			// nicht pausiert, damit laufende Jobs fertig werden und Kontingent freigeben
			run, why = true, "Kontingentdruck: "+pressure+" wartet"
		}
		switch {
		case run && vm.State == "stopped":
			out = append(out, Action{Kind: Start, Workload: w.ID, Zone: vm.Zone, VMID: vm.ID, Name: vm.Name, Reason: why})
		case !run && vm.State == "running":
			out = append(out, Action{Kind: Stop, Workload: w.ID, Zone: vm.Zone, VMID: vm.ID, Name: vm.Name, Reason: why})
		}
	}
	return out
}

// reserve hält Kontingent für noch nicht freigegebene Jobs frei, die bald starten müssen: spätester Start
// = Deadline − Puffer − kürzeste Gesamtdauer. Reserviert wird die Option mit den wenigsten vCPU, die das noch
// schafft. Sonst belegen flexible CPU-Jobs alles, und ein knapper GPU-Job wie w-09 findet zu spät Platz.
func reserve(s *spec.Spec, in Input, p Params, wls []spec.Workload, u *usage) {
	for _, w := range wls {
		j := in.Jobs[w.ID]
		release := s.Hours(w.Release)
		if j.Delivered || len(j.VMs) > 0 || in.Now >= release-1 {
			continue
		}
		var pick spec.Profile
		latest, found := 0.0, false
		for _, o := range s.Options(w) {
			total := float64(o.Profile.BootS+w.InputS+w.OutputS)/3600 + spec.ComputeHours(w, o.Profile, 1, w.RequiredWork)
			ls := s.Hours(w.Deadline) - p.MarginH - total
			if ls < release {
				continue
			}
			if !found || o.Profile.CPUCores < pick.CPUCores || (o.Profile.CPUCores == pick.CPUCores && ls < latest) {
				pick, latest, found = o.Profile, ls, true
			}
		}
		if found && latest-in.Now <= p.ReserveH {
			u.add(pick)
		}
	}
}

// quotaPressure nennt einen freigegebenen Job ohne VM, für den gerade keine legale Option ins Kontingent passt.
func quotaPressure(s *spec.Spec, in Input, wls []spec.Workload, u usage) string {
	for _, w := range wls {
		j := in.Jobs[w.ID]
		if j.Delivered || len(j.VMs) > 0 || in.Now < s.Hours(w.Release)-1 {
			continue
		}
		fits := false
		for _, o := range s.Options(w) {
			if u.fits(s, o.Profile) {
				fits = true
				break
			}
		}
		if !fits {
			return w.ID
		}
	}
	return ""
}

// placeNew wählt für einen Job ohne VM die legale Option mit den geringsten erwarteten Emissionen. Schafft
// keine Option die Deadline mehr, nimmt sie die schnellste, die ins Kontingent passt: Teilfortschritt zählt.
func placeNew(s *spec.Spec, in Input, p Params, w spec.Workload, u *usage, release, deadline, remaining float64) (Action, bool) {
	if in.Now < release-1 { // Grundgerüst: frühestens eine Stunde vor dem Release anlegen
		return Action{}, false
	}
	start := math.Max(in.Now, release)
	best, bestCost := spec.Option{}, math.Inf(1)
	rescue, rescueH := spec.Option{}, math.Inf(1)
	for _, o := range s.Options(w) {
		if !u.fits(s, o.Profile) {
			continue
		}
		compute := spec.ComputeHours(w, o.Profile, 1, remaining)
		setup := float64(o.Profile.BootS+w.InputS) / 3600
		total := setup + compute + float64(w.OutputS)/3600
		if total < rescueH {
			rescue, rescueH = o, total
		}
		if start+total+p.MarginH > deadline {
			continue
		}
		window := int(math.Floor(deadline - p.MarginH - start - setup))
		g := cheapest(in.Forecast[o.Zone.Area], window, spec.Ceil(compute), p.FallbackGCO)
		// gleich effiziente, aber größere VMs blockieren Kontingent, das andere Jobs brauchen
		cost := o.Profile.ActiveW / 1000 * o.Zone.PUE * compute * g *
			(1 + p.QuotaCost*float64(o.Profile.CPUCores)/float64(s.MaxVCPUs))
		if cost < bestCost {
			best, bestCost = o, cost
		}
	}
	reason := fmt.Sprintf("günstigste Option, erwartet %.0f g", bestCost)
	if math.IsInf(bestCost, 1) {
		if math.IsInf(rescueH, 1) {
			return Action{}, false // nichts passt ins Kontingent
		}
		best, reason = rescue, fmt.Sprintf("Deadline knapp: schnellste Option, %.1f h", rescueH)
	}
	u.add(best.Profile)
	return Action{Kind: Create, Workload: w.ID, Zone: best.Zone.Slug, Family: best.Profile.Family, Size: best.Profile.Size,
		Name: Name(w.ID, best.Profile.Family, best.Profile.Size), Reason: reason}, true
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
