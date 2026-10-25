// Package planner decides on every tick which VMs are created, started, stopped or deleted.
// Plan is pure (no clock, no network): same input, same actions. That makes it testable and fast.
//
// Local strategy of the original skeleton (Params.Global=false; the default is the global plan in global.go):
//   - Where: legal (zone, profile) option with the lowest expected emissions
//     = active power × PUE × compute hours × mean of the cheapest forecast hours.
//   - When: a VM computes in the cheapest k hours of the window (k = hours needed) and is stopped in the
//     others (0 W, progress is kept). With little slack it runs straight through.
//   - Cleanup: delete VMs as soon as final_output_delivered holds.
//
// Open (TODO): several workers per job, forecast correction from our own observations. Quota planning,
// parking and just-in-time creation now live in global.go.
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

// VM is one of our own VMs with the profile its name encodes.
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
	Now      float64              // hours since T0 (from virtual_time)
	Jobs     map[string]Job       // per workload ID
	Forecast map[string][]float64 // per power area: g/kWh from the current hour, index 0 = now
}

type Params struct {
	MarginH     float64 // buffer before the deadline (h)
	NoPauseH    float64 // slack below which we no longer pause (h)
	FallbackGCO float64 // assumption for hours without forecast (g/kWh)
	ReserveH    float64 // jobs that must start within this many hours reserve their quota
	QuotaCost   float64 // cost surcharge per share of the vCPU quota (0.5: 144 of 320 vCPU = +22 %)
	SetupCost   bool    // include boot and input at active power in the placement cost
	DeferCreate bool    // create jobs with slack only in one of their clean hours (stopped VMs hold quota)
	// Tile/TileRun extend the forecast over the whole window as a daily pattern (profile choice and pausing).
	// Off: in the simulator the planner then picks faster, larger profiles and crowds late jobs out of the
	// quota (200 seeds: 10/200 instead of 200/200 on time). Needs a global quota plan first.
	Tile    bool
	TileRun bool
	// Global: plan all jobs together with quota per hour (global.go); LifeCost in g per occupied
	// vCPU hour, one GPU card counts as GPUWeight vCPU. Cache holds the plan of one hour. Simulator,
	// 200 seeds: +150 ± 15 points, 13.5 instead of 14.1 kg, 200/200 on time (LifeCost 0–0.03, Shrink 0 best).
	Global     bool
	LifeCost   float64
	GPUWeight  int
	Shrink     float64 // global: share of the daily pattern beyond the forecast (0 = mean only)
	MinCores   int     // global: do not use CPU profiles with fewer cores (0 = all)
	AllEnds    bool    // global: check every end (heap) instead of EndSlices samples
	GPUFirst   bool    // global: plan GPU jobs before CPU jobs in every order
	Orders     int     // global: try this many job orders, take the cheapest plan (≤ 1: deadline order only)
	MinGPUs    int     // global: do not use GPU profiles with fewer cards (0 = all)
	NoPressure bool    // global: ignore quota pressure (simulator diagnostics only)
	JustInTime bool    // global: create new VMs only shortly before their first planned compute hour
	ParkGapH   float64 // global: delete the VM if the next planned compute hour is this far away (0 = off)
	ParkLossH  float64 // global: only park if at most this much compute time since the last checkpoint is lost
	EndSlices  int     // global: this many lifetimes per start are checked (search granularity)
	Cache      *GlobalCache
}

func DefaultParams() Params {
	// ReserveH off: with QuotaCost the reservation scores lower in the simulator (cmd/sim, 100 seeds)
	return Params{MarginH: 2, NoPauseH: 2, FallbackGCO: 250, ReserveH: -1, QuotaCost: 1, SetupCost: true,
		Global: true, LifeCost: 0.01, GPUWeight: 8, Shrink: 0, EndSlices: 6, AllEnds: true, Orders: 48}
}

// Name encodes workload and profile so a restarted orchestrator recognises its own VMs.
func Name(workload, family, size string) string {
	return "gx-" + workload + "--" + family + "--" + size
}

// ParseName is the inverse of Name; ok is false for foreign VMs.
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

// horizon returns the forecast values for the next "window" hours. With tile the forecast is extended beyond
// its end as a daily pattern: the weather there is unknown, but the daily cycle (solar) is not. Without tile
// it ends with the forecast; then no job with more compute hours than forecast hours can pause.
func horizon(fc []float64, window int, tile bool) []float64 {
	if window <= 0 || len(fc) == 0 {
		return nil
	}
	if !tile || window <= len(fc) {
		return slices.Clone(fc[:min(window, len(fc))])
	}
	day := fc[:min(24, len(fc))]
	out := slices.Clone(fc)
	for i := len(fc); i < window; i++ {
		out = append(out, day[i%len(day)])
	}
	return out
}

// cheapest is the mean of the k cheapest forecast hours in fc[:window]. If the job needs more hours than the
// forecast covers, the rest count at the forecast mean; a fixed substitute would systematically penalise long
// runtimes (frugal, slow profiles). fallback only without any forecast.
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
	if p.Global {
		return planGlobal(s, in, p)
	}
	var out []Action
	u := usage{gpus: map[string]int{}}
	for _, j := range in.Jobs {
		for _, vm := range j.VMs {
			if prof, ok := s.Profile(vm.Family, vm.Size); ok {
				u.add(prof)
			}
		}
	}
	// Tightest deadline first: those jobs get quota before the flexible ones.
	wls := slices.Clone(s.Workloads)
	sort.SliceStable(wls, func(a, b int) bool { return wls[a].Deadline.Before(wls[b].Deadline) })
	pressure := quotaPressure(s, in, wls, u)
	reserve(s, in, p, wls, &u)

	for _, w := range wls {
		j := in.Jobs[w.ID]
		if j.Delivered {
			for _, vm := range j.VMs {
				out = append(out, Action{Kind: Delete, Workload: w.ID, Zone: vm.Zone, VMID: vm.ID, Name: vm.Name, Reason: "delivered"})
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
		vm := j.VMs[0] // skeleton: one worker per job
		if vm.State == "creating" || vm.ID == "" {
			continue
		}
		prof, ok := s.Profile(vm.Family, vm.Size)
		if !ok {
			continue
		}
		run, why := shouldRun(s, in, p, w, prof, vm.Zone, release, deadline, remaining)
		if !run && pressure != "" {
			// stopped VMs keep their quota: as long as a released job finds no room we do not pause,
			// so running jobs finish and free quota
			run, why = true, "quota pressure: "+pressure+" waiting"
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

// reserve keeps quota free for not yet released jobs that must start soon: latest start
// = deadline − buffer − shortest total duration. It reserves the option with the fewest vCPU that still
// makes it. Otherwise flexible CPU jobs take everything and a tight GPU job like w-09 finds room too late.
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

// quotaPressure names a released job without a VM for which no legal option currently fits the quota.
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

// placeNew picks the legal option with the lowest expected emissions for a job without a VM. If no option
// can meet the deadline any more, it takes the fastest one that fits the quota: partial progress counts.
func placeNew(s *spec.Spec, in Input, p Params, w spec.Workload, u *usage, release, deadline, remaining float64) (Action, bool) {
	if in.Now < release-1 { // skeleton: create at most one hour before the release
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
		fc := in.Forecast[o.Zone.Area]
		g := cheapest(horizon(fc, window, p.Tile), window, spec.Ceil(compute), p.FallbackGCO)
		// boot and input run now at active power; equally efficient but larger VMs block
		// quota other jobs need
		now := 0.0
		if p.SetupCost {
			now = p.FallbackGCO
			if fc := in.Forecast[o.Zone.Area]; len(fc) > 0 {
				now = fc[0]
			}
		}
		cost := o.Profile.ActiveW / 1000 * o.Zone.PUE * (compute*g + setup*now) *
			(1 + p.QuotaCost*float64(o.Profile.CPUCores)/float64(s.MaxVCPUs))
		if cost < bestCost {
			best, bestCost = o, cost
		}
	}
	reason := fmt.Sprintf("cheapest option, expected %.0f g", bestCost)
	if p.DeferCreate && !math.IsInf(bestCost, 1) {
		compute := spec.ComputeHours(w, best.Profile, 1, remaining)
		total := float64(best.Profile.BootS+w.InputS+w.OutputS)/3600 + compute
		slack := deadline - p.MarginH - start - total
		if fc := in.Forecast[best.Zone.Area]; slack >= p.NoPauseH && len(fc) > 0 {
			vals := horizon(fc, int(math.Floor(deadline-p.MarginH-in.Now)), p.Tile)
			if k := spec.Ceil(compute); k < len(vals) {
				sort.Float64s(vals)
				if fc[0] > vals[k-1] {
					return Action{}, false // later, in a clean hour; until then the quota stays free
				}
			}
		}
	}
	if math.IsInf(bestCost, 1) {
		if math.IsInf(rescueH, 1) {
			return Action{}, false // nothing fits the quota
		}
		best, reason = rescue, fmt.Sprintf("deadline tight: fastest option, %.1f h", rescueH)
	}
	u.add(best.Profile)
	return Action{Kind: Create, Workload: w.ID, Zone: best.Zone.Slug, Family: best.Profile.Family, Size: best.Profile.Size,
		Name: Name(w.ID, best.Profile.Family, best.Profile.Size), Reason: reason}, true
}

// shouldRun: does the VM run now? Yes if slack is tight or the current hour is among the
// cheapest k hours until the deadline (within the forecast).
func shouldRun(s *spec.Spec, in Input, p Params, w spec.Workload, prof spec.Profile, zone string, release, deadline, remaining float64) (bool, string) {
	if in.Now < release {
		return true, "before release: load input"
	}
	compute := spec.ComputeHours(w, prof, 1, remaining)
	need := compute + float64(w.OutputS)/3600
	slack := deadline - p.MarginH - in.Now - need
	if slack < p.NoPauseH || compute <= 0 {
		return true, fmt.Sprintf("slack %.1f h", slack)
	}
	fc := in.Forecast[s.Zones[zone].Area]
	if len(fc) == 0 {
		return true, "no forecast"
	}
	vals := horizon(fc, int(math.Floor(deadline-p.MarginH-in.Now)), p.TileRun)
	window := len(vals)
	k := spec.Ceil(compute)
	if k >= window {
		return true, "needs the whole forecast window"
	}
	sort.Float64s(vals)
	limit := vals[k-1]
	if fc[0] <= limit {
		return true, fmt.Sprintf("now %.0f ≤ threshold %.0f g/kWh", fc[0], limit)
	}
	return false, fmt.Sprintf("now %.0f > threshold %.0f g/kWh, slack %.1f h", fc[0], limit, slack)
}
