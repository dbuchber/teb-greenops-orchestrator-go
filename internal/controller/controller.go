// Package controller runs the loop: fetch status, fetch forecasts at each new hour, plan, execute actions
// in parallel. At 720× 1 s of real time = 12 simulated minutes; so everything that has to wait runs
// concurrently, and the loop never blocks on a single slow call longer than necessary.
package controller

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/api"
	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/planner"
	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/spec"
)

type Controller struct {
	Spec   *spec.Spec
	Work   *api.Work
	Grid   *api.Grid
	Cloud  *api.Cloud
	Params planner.Params
	Log    *slog.Logger
	Dry    bool          // read only and print the first decision, create nothing (clock does not start)
	Tick   time.Duration // interval between status polls (budget: 5/s, burst 10)

	vms       map[string]planner.VM // own VMs by ID
	pending   map[string]time.Time  // VM name -> Create sent, ID not known yet
	forecasts map[string][]float64
	fcHour    int
}

func (c *Controller) Run(ctx context.Context) error {
	c.vms, c.pending, c.fcHour = map[string]planner.VM{}, map[string]time.Time{}, -1
	if c.Tick == 0 {
		c.Tick = 250 * time.Millisecond
	}
	if err := c.Cloud.Discover(ctx); err != nil {
		return err
	}
	if err := c.adopt(ctx); err != nil {
		return err
	}
	areas := c.areas()
	for {
		st, err := c.Work.Status(ctx)
		if err != nil {
			var rl api.RateLimited
			if !errors.As(err, &rl) {
				c.Log.Warn("status", "err", err)
			}
			if !sleep(ctx, time.Second) {
				return ctx.Err()
			}
			continue
		}
		if st.Qualification == "DISQUALIFIED" {
			return errors.New("run disqualified")
		}
		if st.Ended() {
			c.Log.Info("run ended", "state", st.State)
			return nil
		}
		now := c.Spec.Hours(st.VirtualTime)
		if h := int(math.Floor(now)); h != c.fcHour || c.forecasts == nil {
			fc, errs := c.Grid.Forecasts(ctx, areas)
			for a, e := range errs {
				c.Log.Warn("forecast", "area", a, "err", e) // the previous forecast of this run stays in use
			}
			c.merge(fc, now)
			c.fcHour = h
			// for the post-run analysis: forecast error against the actual values from the portal export
			logged := map[string][]int{}
			for a, v := range c.forecasts {
				for _, x := range v {
					logged[a] = append(logged[a], int(math.Round(x)))
				}
			}
			c.Log.Info("forecast", "hour", h, "values", logged)
		}
		in := c.input(st, now)
		acts := planner.Plan(c.Spec, in, c.Params)
		if c.Dry {
			c.dryReport(st, now, acts)
			return nil
		}
		acts = append(acts, c.clock(st)...)
		c.execute(ctx, acts)
		if c.done(st) {
			c.Log.Info("all workloads delivered, all VMs deleted")
			return nil
		}
		if !sleep(ctx, c.Tick) {
			return ctx.Err()
		}
	}
}

// The clock VM starts the clock of a READY run: the clock only runs from the first accepted Create, but the
// first workloads are released only after a few hours. It is bound to the workload with the latest release
// (otherwise the portal reports "VM is missing a workload binding"), only loads input briefly and is deleted
// as soon as the clock runs; no progress is lost.
const clockWorkload = "clock"

func isClock(name string) bool {
	w, _, _, ok := planner.ParseName(name)
	return ok && w == clockWorkload
}

// clockOption: legal option of the latest workload with the smallest active power × PUE.
func (c *Controller) clockOption() (spec.Workload, spec.Option, bool) {
	var w spec.Workload
	for _, x := range c.Spec.Workloads {
		if w.ID == "" || x.Release.After(w.Release) {
			w = x
		}
	}
	var best spec.Option
	found := false
	for _, o := range c.Spec.Options(w) {
		if !found || o.Profile.ActiveW*o.Zone.PUE < best.Profile.ActiveW*best.Zone.PUE {
			best, found = o, true
		}
	}
	return w, best, found
}

func (c *Controller) clock(st api.Status) []planner.Action {
	var own []planner.VM
	for _, vm := range c.vms {
		if isClock(vm.Name) {
			own = append(own, vm)
		}
	}
	sent := false
	for name := range c.pending {
		sent = sent || isClock(name)
	}
	switch {
	case st.State == "READY" && len(own) == 0 && !sent && len(c.vms) == 0:
		w, o, ok := c.clockOption()
		if !ok {
			return nil
		}
		return []planner.Action{{Kind: planner.Create, Workload: w.ID, Zone: o.Zone.Slug, Family: o.Profile.Family,
			Size: o.Profile.Size, Name: planner.Name(clockWorkload, o.Profile.Family, o.Profile.Size), Reason: "start clock"}}
	case st.State != "READY":
		var out []planner.Action
		for _, vm := range own {
			if vm.ID != "" {
				out = append(out, planner.Action{Kind: planner.Delete, Zone: vm.Zone, VMID: vm.ID, Name: vm.Name, Reason: "clock running"})
			}
		}
		return out
	}
	return nil
}

func (c *Controller) dryReport(st api.Status, now float64, acts []planner.Action) {
	areas := make([]string, 0, len(c.forecasts))
	for a, v := range c.forecasts {
		areas = append(areas, a+":"+strconv.Itoa(len(v))+"h")
	}
	sort.Strings(areas)
	c.Log.Info("dry: run", "state", st.State, "speed", st.Speed, "qualification", st.Qualification,
		"virtual_time", st.VirtualTime, "hour", now, "workloads", len(st.Workloads), "own_vms", len(c.vms),
		"forecasts", strings.Join(areas, " "))
	for _, a := range acts {
		c.Log.Info("dry: action", "action", a.Kind, "workload", a.Workload, "zone", a.Zone, "profile", a.Family+"."+a.Size, "reason", a.Reason)
	}
	if st.State == "READY" {
		c.Log.Info("dry: in a real run a clock VM starts the clock first and is deleted afterwards")
	}
}

func (c *Controller) areas() []string {
	seen := map[string]bool{}
	var out []string
	for _, z := range c.Spec.Zones {
		if !seen[z.Area] {
			seen[z.Area] = true
			out = append(out, z.Area)
		}
	}
	sort.Strings(out)
	return out
}

// merge stores the forecasts so that index 0 is the current hour.
func (c *Controller) merge(fc map[string][]api.Point, now float64) {
	if c.forecasts == nil {
		c.forecasts = map[string][]float64{}
	}
	cur := c.Spec.T0.Add(time.Duration(math.Floor(now)) * time.Hour)
	for area, pts := range fc {
		var vals []float64
		for _, p := range pts {
			if !p.Time.Before(cur) {
				vals = append(vals, p.GCO2kW)
			}
		}
		if len(vals) > 0 {
			c.forecasts[area] = vals
		}
	}
}

// adopt takes over own VMs (name gx-...) after a restart instead of creating them twice.
func (c *Controller) adopt(ctx context.Context) error {
	vms, err := c.Cloud.List(ctx)
	if err != nil {
		return err
	}
	for _, v := range vms {
		if _, f, s, ok := planner.ParseName(v.Name); ok {
			c.vms[v.ID] = planner.VM{ID: v.ID, Name: v.Name, Zone: v.Zone, Family: f, Size: s, State: v.State}
			c.Log.Info("adopted", "vm", v.Name, "zone", v.Zone)
		}
	}
	return nil
}

func (c *Controller) input(st api.Status, now float64) planner.Input {
	jobs := map[string]planner.Job{}
	live := map[string]api.Assignment{}
	for _, w := range st.Workloads {
		for _, a := range w.Assignments {
			live[a.VMID] = a
		}
	}
	byWorkload := map[string][]planner.VM{}
	for id, vm := range c.vms {
		if a, ok := live[id]; ok {
			vm.State = a.State // the work status is authoritative
			c.vms[id] = vm
		}
		wid, _, _, _ := planner.ParseName(vm.Name)
		byWorkload[wid] = append(byWorkload[wid], vm)
	}
	for name := range c.pending { // Create sent, ID not known yet: count as "creating"
		wid, f, s, _ := planner.ParseName(name)
		byWorkload[wid] = append(byWorkload[wid], planner.VM{Name: name, Family: f, Size: s, State: "creating"})
	}
	for _, w := range st.Workloads {
		jobs[w.ID] = planner.Job{Completed: w.Completed(), Delivered: w.Delivered, VMs: byWorkload[w.ID]}
	}
	return planner.Input{Now: now, Jobs: jobs, Forecast: c.forecasts}
}

// execute runs all actions in parallel; deletes first so quota frees up for creates.
func (c *Controller) execute(ctx context.Context, acts []planner.Action) {
	sort.SliceStable(acts, func(a, b int) bool { return acts[a].Kind == planner.Delete && acts[b].Kind != planner.Delete })
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, a := range acts {
		if a.Kind == planner.Create {
			// second legality check right before the Create: an illegal attempt disqualifies the run.
			w, ok1 := c.Spec.Workload(a.Workload)
			p, ok2 := c.Spec.Profile(a.Family, a.Size)
			if !ok1 || !ok2 || c.Spec.Legal(w, a.Zone, p) != nil {
				c.Log.Error("create dropped: not legal", "workload", a.Workload, "zone", a.Zone)
				continue
			}
			c.pending[a.Name] = time.Now()
		}
		wg.Add(1)
		go func(a planner.Action) {
			defer wg.Done()
			var err error
			var id string
			switch a.Kind {
			case planner.Create:
				id, err = c.Cloud.Create(ctx, a.Zone, a.Family, a.Size, a.Name, a.Workload)
			case planner.Start:
				err = c.Cloud.Start(ctx, a.Zone, a.VMID)
			case planner.Stop:
				err = c.Cloud.Stop(ctx, a.Zone, a.VMID)
			case planner.Delete:
				err = c.Cloud.Delete(ctx, a.Zone, a.VMID)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				c.Log.Warn("action failed", "action", a.Kind, "workload", a.Workload, "err", err)
				if a.Kind == planner.Create {
					delete(c.pending, a.Name) // re-planned on the next tick
				}
				return
			}
			c.Log.Info("action", "action", a.Kind, "workload", a.Workload, "zone", a.Zone, "reason", a.Reason)
			switch a.Kind {
			case planner.Create:
				if id != "" {
					delete(c.pending, a.Name)
					c.vms[id] = planner.VM{ID: id, Name: a.Name, Zone: a.Zone, Family: a.Family, Size: a.Size, State: "creating"}
				}
			case planner.Delete:
				delete(c.vms, a.VMID)
			}
		}(a)
	}
	wg.Wait()
	if len(c.pending) > 0 { // fill in IDs of unknown creates by name
		if vms, err := c.Cloud.List(ctx); err == nil {
			for _, v := range vms {
				if _, ok := c.pending[v.Name]; ok {
					_, f, s, _ := planner.ParseName(v.Name)
					c.vms[v.ID] = planner.VM{ID: v.ID, Name: v.Name, Zone: v.Zone, Family: f, Size: s, State: v.State}
					delete(c.pending, v.Name)
				}
			}
		}
	}
}

func (c *Controller) done(st api.Status) bool {
	for _, w := range st.Workloads {
		if !w.Delivered {
			return false
		}
	}
	return len(c.vms) == 0 && len(c.pending) == 0
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
