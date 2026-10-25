package planner

// Global quota planning: plans all jobs together over the remaining window.
//
// A VM holds quota from its Create until its deletion, even when stopped. So every job gets a lifetime
// [start, end) and computes within it in its cleanest hours: a longer lifetime means more choice of clean
// hours but more occupied quota. Cost = expected emissions + LifeCost per occupied vCPU hour (one GPU card
// counts as GPUWeight vCPU). Greedy: existing VMs first, then new jobs by deadline; jobs that find no room
// move to the front in the next pass. Beyond the end of the 24 h forecast it is extended as a daily
// pattern (horizon).

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"

	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/spec"
)

// GlobalCache holds the plan of one hour; it re-plans only when a new hour begins.
type GlobalCache struct {
	hour     int
	plans    map[string]jobPlan
	pressure string
}

type jobPlan struct {
	opt        spec.Option
	start, end int          // absolute hours: Create to deletion
	run        map[int]bool // absolute hours in which it computes
	cost       float64
}

// occupancy is the planned quota usage per hour from h0.
type occupancy struct {
	h0         int
	vms, vcpus []int
	gpus       map[string][]int
}

func newOccupancy(h0, n int) *occupancy {
	return &occupancy{h0: h0, vms: make([]int, n), vcpus: make([]int, n), gpus: map[string][]int{}}
}

func (o *occupancy) fits(s *spec.Spec, p spec.Profile, from, to int) bool {
	g := o.gpus[p.Family]
	for h := max(from, o.h0); h < to && h-o.h0 < len(o.vms); h++ {
		i := h - o.h0
		if o.vms[i]+1 > s.MaxVMs || o.vcpus[i]+p.CPUCores > s.MaxVCPUs {
			return false
		}
		if p.GPUCount > 0 {
			used := 0
			if g != nil {
				used = g[i]
			}
			if used+p.GPUCount > s.GPUQuota[p.Family] {
				return false
			}
		}
	}
	return true
}

// fitsHour checks a single hour (for extending a lifetime step by step).
func (o *occupancy) fitsHour(s *spec.Spec, p spec.Profile, h int) bool {
	i := h - o.h0
	if i < 0 || i >= len(o.vms) {
		return true
	}
	if o.vms[i]+1 > s.MaxVMs || o.vcpus[i]+p.CPUCores > s.MaxVCPUs {
		return false
	}
	if p.GPUCount > 0 {
		used := 0
		if g := o.gpus[p.Family]; g != nil {
			used = g[i]
		}
		return used+p.GPUCount <= s.GPUQuota[p.Family]
	}
	return true
}

func (o *occupancy) remove(p spec.Profile, from, to int) {
	for h := max(from, o.h0); h < to && h-o.h0 < len(o.vms); h++ {
		i := h - o.h0
		o.vms[i]--
		o.vcpus[i] -= p.CPUCores
		if p.GPUCount > 0 {
			o.gpus[p.Family][i] -= p.GPUCount
		}
	}
}

func (o *occupancy) add(p spec.Profile, from, to int) {
	if p.GPUCount > 0 && o.gpus[p.Family] == nil {
		o.gpus[p.Family] = make([]int, len(o.vms))
	}
	for h := max(from, o.h0); h < to && h-o.h0 < len(o.vms); h++ {
		i := h - o.h0
		o.vms[i]++
		o.vcpus[i] += p.CPUCores
		if p.GPUCount > 0 {
			o.gpus[p.Family][i] += p.GPUCount
		}
	}
}

// best picks option, start and end with the lowest cost for a job that fit the quota.
// fixed: existing VM (option and start are fixed). check: only plans that fit the quota.
func best(s *spec.Spec, in Input, p Params, w spec.Workload, opts []spec.Option, fixed, check, setupDone bool,
	occ *occupancy, series map[string][]float64) (jobPlan, bool) {
	h0 := occ.h0
	release, deadline := s.Hours(w.Release), s.Hours(w.Deadline)
	latestEnd := int(math.Floor(deadline - p.MarginH))
	remaining := math.Max(w.RequiredWork-in.Jobs[w.ID].Completed, 0)
	outH := int(math.Ceil(float64(w.OutputS) / 3600))
	var bestPlan jobPlan
	bestCost := math.Inf(1)
	vals := make([]float64, 0, 128)
	heap := make([]float64, 0, 128)
	for _, o := range opts {
		ser := series[o.Zone.Area]
		at := func(h int) float64 {
			if i := h - h0; i >= 0 && i < len(ser) {
				return ser[i]
			}
			return p.FallbackGCO
		}
		compute := spec.ComputeHours(w, o.Profile, 1, remaining)
		k := int(math.Ceil(compute - 1e-9))
		setup := float64(o.Profile.BootS+w.InputS) / 3600
		if setupDone {
			setup = 0
		}
		weight := float64(o.Profile.CPUCores + p.GPUWeight*o.Profile.GPUCount)
		kw := o.Profile.ActiveW / 1000 * o.Zone.PUE
		first := h0
		if !fixed {
			first = max(h0, int(math.Floor(release))-1)
		}
		for st := first; ; {
			cs := max(st+int(math.Ceil(setup-1e-9)), int(math.Ceil(release-1e-9)), h0)
			minEnd := cs + k + outH
			if minEnd > latestEnd {
				break
			}
			if p.AllEnds && k > 0 {
				// all ends in one pass: max-heap of the k cleanest hours, running sum; once an hour is
				// too full, no later end fits either
				heap = heap[:0]
				sum := 0.0
				room := !check || occ.fits(s, o.Profile, st, minEnd)
				for h := cs; room && h+1+outH <= latestEnd; h++ {
					v := at(h)
					switch {
					case len(heap) < k:
						heap = append(heap, v)
						sum += v
						for i := len(heap) - 1; i > 0 && heap[(i-1)/2] < heap[i]; i = (i - 1) / 2 {
							heap[i], heap[(i-1)/2] = heap[(i-1)/2], heap[i]
						}
					case v < heap[0]:
						sum += v - heap[0]
						heap[0] = v
						for i := 0; ; {
							l, r, m := 2*i+1, 2*i+2, i
							if l < len(heap) && heap[l] > heap[m] {
								m = l
							}
							if r < len(heap) && heap[r] > heap[m] {
								m = r
							}
							if m == i {
								break
							}
							heap[i], heap[m] = heap[m], heap[i]
							i = m
						}
					}
					e := h + 1 + outH
					if e < minEnd {
						continue
					}
					if e > minEnd && check && !occ.fitsHour(s, o.Profile, e-1) {
						break
					}
					cost := kw*(sum+setup*at(st)) + p.LifeCost*weight*float64(e-st)
					if cost < bestCost {
						bestCost = cost
						bestPlan = jobPlan{opt: o, start: st, end: e, cost: cost}
					}
				}
			} else {
				step := max(1, (latestEnd-minEnd)/max(p.EndSlices, 1))
				for e := minEnd; e <= latestEnd; e += step {
					if e+step > latestEnd && e != latestEnd {
						e = latestEnd // always check the latest end
					}
					if check && !occ.fits(s, o.Profile, st, e) {
						continue
					}
					vals = vals[:0]
					for h := cs; h < e-outH; h++ {
						vals = append(vals, at(h))
					}
					sort.Float64s(vals)
					sum := 0.0
					for _, v := range vals[:k] {
						sum += v
					}
					cost := kw*(sum+setup*at(st)) + p.LifeCost*weight*float64(e-st)
					if cost < bestCost {
						bestCost = cost
						bestPlan = jobPlan{opt: o, start: st, end: e, cost: cost}
					}
				}
			}
			if fixed {
				break
			}
			if st-first < 6 {
				st++
			} else {
				st += 3
			}
		}
	}
	if math.IsInf(bestCost, 1) {
		return jobPlan{}, false
	}
	// compute hours of the chosen plan: the k cleanest in the window
	o, b := bestPlan.opt, &bestPlan
	ser := series[o.Zone.Area]
	compute := spec.ComputeHours(w, o.Profile, 1, remaining)
	k := int(math.Ceil(compute - 1e-9))
	setup := 0.0
	if !setupDone {
		setup = float64(o.Profile.BootS+w.InputS) / 3600
	}
	cs := max(b.start+int(math.Ceil(setup-1e-9)), int(math.Ceil(release-1e-9)), h0)
	hours := make([]int, 0, b.end-cs)
	for h := cs; h < b.end-outH; h++ {
		hours = append(hours, h)
	}
	val := func(h int) float64 {
		if i := h - h0; i >= 0 && i < len(ser) {
			return ser[i]
		}
		return p.FallbackGCO
	}
	sort.SliceStable(hours, func(a, c int) bool { return val(hours[a]) < val(hours[c]) })
	b.run = map[int]bool{}
	for _, h := range hours[:min(k, len(hours))] {
		b.run[h] = true
	}
	return bestPlan, true
}

// greedy plans in the given order; result: plans, unplaced jobs, total cost.
func greedy(s *spec.Spec, in Input, p Params, h0, n int, existing, order []spec.Workload,
	series map[string][]float64) (map[string]jobPlan, []string, float64) {
	// Existing VMs first book only their shortest lifetime (run straight through); only after the new jobs
	// are placed may they live longer and pause with the remaining quota. Otherwise their pauses crowd out
	// the jobs that start later.
	shortest := p
	shortest.LifeCost = 1e9
	var plans map[string]jobPlan
	var occ *occupancy
	unplaced := []string{}
	for pass := 0; pass < 3; pass++ {
		occ = newOccupancy(h0, n)
		plans = map[string]jobPlan{}
		for _, w := range existing {
			vm := in.Jobs[w.ID].VMs[0]
			prof, ok := s.Profile(vm.Family, vm.Size)
			if !ok {
				continue
			}
			opt := spec.Option{Zone: s.Zones[vm.Zone], Profile: prof}
			jp, ok := best(s, in, shortest, w, []spec.Option{opt}, true, false, vm.State != "creating", occ, series)
			if !ok { // too late for any plan: run straight through
				jp = jobPlan{opt: opt, start: h0, end: Hours120, run: map[int]bool{}}
				for h := h0; h <= Hours120; h++ {
					jp.run[h] = true
				}
			}
			occ.add(prof, h0, jp.end)
			plans[w.ID] = jp
		}
		var missing []spec.Workload
		unplaced = unplaced[:0]
		for _, w := range order {
			jp, ok := best(s, in, p, w, options(s, w, p), false, true, false, occ, series)
			if !ok {
				missing = append(missing, w)
				unplaced = append(unplaced, w.ID)
				continue
			}
			occ.add(jp.opt.Profile, jp.start, jp.end)
			plans[w.ID] = jp
		}
		if len(missing) == 0 {
			break
		}
		// jobs that found no room move to the front in the next pass
		rest := slices.DeleteFunc(slices.Clone(order), func(w spec.Workload) bool {
			return slices.ContainsFunc(missing, func(m spec.Workload) bool { return m.ID == w.ID })
		})
		order = append(missing, rest...)
	}
	if len(unplaced) == 0 {
		for _, w := range existing {
			short, ok := plans[w.ID]
			if !ok {
				continue
			}
			vm := in.Jobs[w.ID].VMs[0]
			occ.remove(short.opt.Profile, h0, short.end)
			jp, ok := best(s, in, p, w, []spec.Option{short.opt}, true, true, vm.State != "creating", occ, series)
			if !ok {
				jp = short
			}
			occ.add(jp.opt.Profile, h0, jp.end)
			plans[w.ID] = jp
		}
	}
	total := 0.0
	for _, jp := range plans {
		total += jp.cost
	}
	return plans, unplaced, total
}

// globalPlan plans all open jobs for hour h0.
func globalPlan(s *spec.Spec, in Input, p Params, h0 int) (map[string]jobPlan, string) {
	n := Hours120 - h0 + 1
	series := map[string][]float64{}
	for a, fc := range in.Forecast {
		ser := horizon(fc, n, true)
		// beyond the forecast only the daily cycle is certain: shrink towards the mean (Shrink 1 = full pattern)
		if len(fc) > 0 && len(ser) > len(fc) {
			m := 0.0
			for _, v := range fc {
				m += v
			}
			m /= float64(len(fc))
			for i := len(fc); i < len(ser); i++ {
				ser[i] = m + p.Shrink*(ser[i]-m)
			}
		}
		series[a] = ser
	}
	wls := slices.Clone(s.Workloads)
	sort.SliceStable(wls, func(a, b int) bool { return wls[a].Deadline.Before(wls[b].Deadline) })
	var existing, fresh []spec.Workload
	for _, w := range wls {
		j := in.Jobs[w.ID]
		switch {
		case j.Delivered:
		case len(j.VMs) > 0:
			existing = append(existing, w)
		default:
			fresh = append(fresh, w)
		}
	}
	if p.GPUFirst {
		// GPU jobs first: they have few legal zones and large emissions; the order within each
		// group stays (deadline or random)
		sort.SliceStable(fresh, func(a, b int) bool { return isGPU(fresh[a]) && !isGPU(fresh[b]) })
	}
	plans, unplaced, bestTotal := greedy(s, in, p, h0, n, existing, fresh, series)
	if p.Orders > 1 && len(fresh) > 1 {
		// other orders: first come gets the quota; keep the plan that places all jobs and costs
		// the least. Randomness is fixed per hour so the plan stays reproducible.
		r := rand.New(rand.NewPCG(uint64(h0), 7))
		for i := 1; i < p.Orders; i++ {
			order := slices.Clone(fresh)
			r.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
			if p.GPUFirst {
				sort.SliceStable(order, func(a, b int) bool { return isGPU(order[a]) && !isGPU(order[b]) })
			}
			pl, un, total := greedy(s, in, p, h0, n, existing, order, series)
			if len(un) < len(unplaced) || (len(un) == len(unplaced) && total < bestTotal-1e-6) {
				plans, unplaced, bestTotal = pl, un, total
			}
		}
	}
	pressure := ""
	for _, id := range unplaced {
		w, _ := s.Workload(id)
		if in.Now >= s.Hours(w.Release)-1 {
			pressure = id
			break
		}
	}
	return plans, pressure
}

// jitDue: is it time for the Create? Yes if boot and input just fit before the first planned compute hour
// (one hour buffer).
func jitDue(w spec.Workload, jp jobPlan, h0 int) bool {
	first := -1
	for h := range jp.run {
		if first < 0 || h < first {
			first = h
		}
	}
	if first < 0 {
		return true
	}
	setup := int(math.Ceil(float64(jp.opt.Profile.BootS+w.InputS)/3600 - 1e-9))
	return h0 >= first-setup-1
}

// park: is it worth deleting the VM of a pausing job? A stopped VM holds quota; after the deletion the job
// re-plans (boot and input again) and resumes from the last checkpoint. Only if the next planned compute hour
// is at least ParkGapH away and at most ParkLossH of compute time since the checkpoint is lost; checkpoints
// occur every CheckpointS seconds of compute time.
func park(w spec.Workload, prof spec.Profile, completed float64, jp jobPlan, h0 int, p Params) (int, bool) {
	if p.ParkGapH <= 0 || w.RestartFromZero || w.CheckpointS <= 0 {
		return 0, false
	}
	next := -1
	for h := range jp.run {
		if h > h0 && (next < 0 || h < next) {
			next = h
		}
	}
	gap := next - h0
	if next >= 0 && float64(gap) < p.ParkGapH {
		return 0, false
	}
	rate := spec.RatePerHour(w, prof, 1)
	chunk := rate * float64(w.CheckpointS) / 3600
	lost := completed - math.Floor(completed/chunk+1e-9)*chunk
	if lost > p.ParkLossH*rate {
		return 0, false
	}
	return gap, true
}

// options are the legal options without CPU profiles below MinCores and GPU profiles below MinGPUs; if none
// remain, all of them.
func options(s *spec.Spec, w spec.Workload, p Params) []spec.Option {
	all := s.Options(w)
	if p.MinCores <= 0 && p.MinGPUs <= 0 {
		return all
	}
	out := slices.DeleteFunc(slices.Clone(all), func(o spec.Option) bool {
		if o.Profile.GPUCount > 0 {
			return o.Profile.GPUCount < p.MinGPUs
		}
		return o.Profile.CPUCores < p.MinCores
	})
	if len(out) == 0 {
		return all
	}
	return out
}

func isGPU(w spec.Workload) bool { return w.WorkUnit == "tflop" }

// Hours120 is the end of the window in hours since T0.
const Hours120 = 120

// planGlobal turns the plan of the current hour into actions.
func planGlobal(s *spec.Spec, in Input, p Params) []Action {
	h0 := int(math.Floor(in.Now + 1e-9))
	c := p.Cache
	if c == nil {
		c = &GlobalCache{hour: -1}
	}
	if c.plans == nil || c.hour != h0 {
		c.plans, c.pressure = globalPlan(s, in, p, h0)
		c.hour = h0
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
	wls := slices.Clone(s.Workloads)
	sort.SliceStable(wls, func(a, b int) bool { return wls[a].Deadline.Before(wls[b].Deadline) })
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
		jp, planned := c.plans[w.ID]
		if len(j.VMs) == 0 {
			if !planned {
				// no room in the plan: as before, the cheapest or fastest option that fits now
				if a, ok := placeNew(s, in, p, w, &u, release, deadline, remaining); ok {
					out = append(out, a)
				}
				continue
			}
			if jp.start > h0 || !u.fits(s, jp.opt.Profile) {
				continue
			}
			if (p.JustInTime || p.ParkGapH > 0) && !jitDue(w, jp, h0) {
				continue // waiting stopped only holds quota; boot and input shortly before the first compute hour
			}
			u.add(jp.opt.Profile)
			out = append(out, Action{Kind: Create, Workload: w.ID, Zone: jp.opt.Zone.Slug, Family: jp.opt.Profile.Family,
				Size: jp.opt.Profile.Size, Name: Name(w.ID, jp.opt.Profile.Family, jp.opt.Profile.Size),
				Reason: fmt.Sprintf("plan: hour %d–%d, %d compute hours, expected %.0f", jp.start, jp.end, len(jp.run), jp.cost)})
			continue
		}
		vm := j.VMs[0]
		if vm.State == "creating" || vm.ID == "" {
			continue
		}
		prof, ok := s.Profile(vm.Family, vm.Size)
		if !ok {
			continue
		}
		need := spec.ComputeHours(w, prof, 1, remaining) + float64(w.OutputS)/3600
		slack := deadline - p.MarginH - in.Now - need
		run, why := false, ""
		switch {
		case in.Now < release:
			run, why = true, "before release: load input"
		case slack < p.NoPauseH || remaining <= 0:
			run, why = true, fmt.Sprintf("slack %.1f h", slack)
		case c.pressure != "" && !p.NoPressure:
			run, why = true, "quota pressure: "+c.pressure+" waiting"
		case planned && jp.run[h0]:
			run, why = true, "planned compute hour"
		case !planned:
			run, why = true, "no plan"
		default:
			why = fmt.Sprintf("pause per plan, slack %.1f h", slack)
		}
		if !run && planned && vm.State == "stopped" && j.Completed > 0 {
			// only VMs that have already computed and are stopped; otherwise the rule parks mid-setup
			if gap, ok := park(w, prof, j.Completed, jp, h0, p); ok {
				out = append(out, Action{Kind: Delete, Workload: w.ID, Zone: vm.Zone, VMID: vm.ID, Name: vm.Name,
					Reason: fmt.Sprintf("park: next compute hour in %d h, quota freed", gap)})
				continue
			}
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
