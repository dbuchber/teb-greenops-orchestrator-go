// Package controller führt die Schleife aus: Status holen, zur neuen Stunde Prognosen holen, planen, Aktionen
// parallel ausführen. Bei 720× ist 1 s echte Zeit = 12 simulierte Minuten; daher läuft alles, was warten
// muss, nebenläufig, und die Schleife blockiert nie auf einem einzelnen langsamen Aufruf länger als nötig.
package controller

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
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
	Dry    bool          // nur lesen und die erste Entscheidung ausgeben, nichts anlegen (Uhr startet nicht)
	Tick   time.Duration // Abstand der Status-Abfragen (Budget: 5/s, burst 10)

	vms       map[string]planner.VM // eigene VMs nach ID
	pending   map[string]time.Time  // VM-Name -> Create gesendet, ID noch unbekannt
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
			return errors.New("Lauf disqualifiziert")
		}
		if st.Ended() {
			c.Log.Info("Lauf beendet", "state", st.State)
			return nil
		}
		now := c.Spec.Hours(st.VirtualTime)
		if h := int(math.Floor(now)); h != c.fcHour || c.forecasts == nil {
			fc, errs := c.Grid.Forecasts(ctx, areas)
			for a, e := range errs {
				c.Log.Warn("prognose", "area", a, "err", e) // alte Prognose dieses Laufs bleibt in Gebrauch
			}
			c.merge(fc, now)
			c.fcHour = h
		}
		in := c.input(st, now)
		acts := planner.Plan(c.Spec, in, c.Params)
		if c.Dry {
			for _, a := range acts {
				c.Log.Info("dry", "action", a.Kind, "workload", a.Workload, "zone", a.Zone, "profile", a.Family+"."+a.Size, "reason", a.Reason)
			}
			return nil
		}
		c.execute(ctx, acts)
		if c.done(st) {
			c.Log.Info("alle Workloads geliefert, alle VMs gelöscht")
			return nil
		}
		if !sleep(ctx, c.Tick) {
			return ctx.Err()
		}
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

// merge legt die Prognosen so ab, dass Index 0 die aktuelle Stunde ist.
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

// adopt übernimmt eigene VMs (Name gx-...) nach einem Neustart, statt sie doppelt anzulegen.
func (c *Controller) adopt(ctx context.Context) error {
	vms, err := c.Cloud.List(ctx)
	if err != nil {
		return err
	}
	for _, v := range vms {
		if _, f, s, ok := planner.ParseName(v.Name); ok {
			c.vms[v.ID] = planner.VM{ID: v.ID, Name: v.Name, Zone: v.Zone, Family: f, Size: s, State: v.State}
			c.Log.Info("übernommen", "vm", v.Name, "zone", v.Zone)
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
			vm.State = a.State // der Work-Status ist maßgeblich
			c.vms[id] = vm
		}
		wid, _, _, _ := planner.ParseName(vm.Name)
		byWorkload[wid] = append(byWorkload[wid], vm)
	}
	for name := range c.pending { // Create gesendet, ID noch unbekannt: als "creating" zählen
		wid, f, s, _ := planner.ParseName(name)
		byWorkload[wid] = append(byWorkload[wid], planner.VM{Name: name, Family: f, Size: s, State: "creating"})
	}
	for _, w := range st.Workloads {
		jobs[w.ID] = planner.Job{Completed: w.Completed(), Delivered: w.Delivered, VMs: byWorkload[w.ID]}
	}
	return planner.Input{Now: now, Jobs: jobs, Forecast: c.forecasts}
}

// execute führt alle Aktionen parallel aus; Löschen zuerst, damit Kontingent für Creates frei wird.
func (c *Controller) execute(ctx context.Context, acts []planner.Action) {
	sort.SliceStable(acts, func(a, b int) bool { return acts[a].Kind == planner.Delete && acts[b].Kind != planner.Delete })
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, a := range acts {
		if a.Kind == planner.Create {
			// zweite Legalitätsprüfung unmittelbar vor dem Create: ein illegaler Versuch disqualifiziert den Lauf
			w, ok1 := c.Spec.Workload(a.Workload)
			p, ok2 := c.Spec.Profile(a.Family, a.Size)
			if !ok1 || !ok2 || c.Spec.Legal(w, a.Zone, p) != nil {
				c.Log.Error("Create verworfen: nicht legal", "workload", a.Workload, "zone", a.Zone)
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
				c.Log.Warn("aktion fehlgeschlagen", "action", a.Kind, "workload", a.Workload, "err", err)
				if a.Kind == planner.Create {
					delete(c.pending, a.Name) // beim nächsten Takt neu geplant
				}
				return
			}
			c.Log.Info("aktion", "action", a.Kind, "workload", a.Workload, "zone", a.Zone, "reason", a.Reason)
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
	if len(c.pending) > 0 { // IDs unbekannter Creates über den Namen nachtragen
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
