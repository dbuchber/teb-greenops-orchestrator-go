package sim

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/planner"
	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/spec"
)

// Modellannahmen (aus den öffentlichen Regeln abgeleitet, Details geschätzt):
//   - Boot, Input (setup) und Rechnen ziehen Wirkleistung, Warten auf Release und Output Leerlaufleistung,
//     gestoppt 0 W; an den Live-Läufen gemessen (runs/*/hourly_impact.csv gegen allocations.csv, R² = 1).
//   - Ein Start nach Stopp bootet nicht neu (in allocations.csv: ein Boot je VM).
//   - Input darf vor dem Release laufen, gerechnet wird erst ab Release.
//   - Stoppen behält alles; Löschen setzt auf den letzten Checkpoint zurück (restart_from_zero: auf null).
//   - Output läuft, sobald die Arbeit fertig ist und eine VM des Jobs läuft.
//   - Energie × PUE × Ist-Intensität der Zone ergibt die Emissionen; ungebundene VMs zählen nur gemeinsam.
type Config struct {
	Seed      uint64
	StepH     float64 // Simulationsschritt (h)
	PlanEvery float64 // Abstand der Planungen (h); 250 ms bei 720× = 0,05 h
	LagH      float64 // Verzögerung bis eine Aktion wirkt (h)
	Params    planner.Params
	Trace     func(now float64, a planner.Action, accepted bool) // optional: jede wirksame Aktion
}

func DefaultConfig(seed uint64) Config {
	return Config{Seed: seed, StepH: 1.0 / 60, PlanEvery: 0.05, LagH: 0.05, Params: planner.DefaultParams()}
}

type JobResult struct {
	Delivered   bool
	DeliveredAt float64
	Progress    float64 // Anteil der Arbeit bis zur Deadline
	CarbonG     float64
}

type Result struct {
	Score        float64
	OnTime       int
	ElectricityG float64
	Disqualified bool
	Jobs         map[string]JobResult
}

// Ref sind die Referenzenergien (kWh) je Workload aus rules.json.
type Ref map[string]float64

func LoadRef(dir string) (Ref, error) {
	var r struct {
		Rules struct {
			Ref map[string]string `json:"reference_energy_kwh"`
		} `json:"rules"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "rules.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	out := Ref{}
	for k, v := range r.Rules.Ref {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, err
		}
		out[k] = f
	}
	return out, nil
}

type vm struct {
	planner.VM
	workload  string
	prof      spec.Profile
	zone      spec.Zone
	bootLeft  float64
	inputLeft float64
	deleted   bool
}

type job struct {
	w            spec.Workload
	completed    float64
	checkpointed float64
	sinceCkpt    float64 // Rechenstunden seit dem letzten Checkpoint
	outputLeft   float64 // < 0: Output noch nicht begonnen
	delivered    bool
	deliveredAt  float64
	atDeadline   float64
	carbon       float64
}

type pending struct {
	at float64
	a  planner.Action
}

// Run simuliert einen Lauf von 120 h mit planner.Plan als Orchestrator.
func Run(s *spec.Spec, ref Ref, cfg Config) Result {
	world := NewWorld(cfg.Seed)
	jobs := map[string]*job{}
	for _, w := range s.Workloads {
		jobs[w.ID] = &job{w: w, outputLeft: -1, atDeadline: -1}
	}
	var vms []*vm
	var queue []pending
	shared := 0.0
	disq := false
	nextID := 0
	nextPlan := 0.0

	apply := func(a planner.Action, now float64) {
		switch a.Kind {
		case planner.Create:
			w, _ := s.Workload(a.Workload)
			p, _ := s.Profile(a.Family, a.Size)
			if s.Legal(w, a.Zone, p) != nil {
				disq = true // wie live: schon der Versuch disqualifiziert
				return
			}
			ok := fits(s, vms, p)
			if cfg.Trace != nil {
				cfg.Trace(now, a, ok)
			}
			if !ok {
				return // die API lehnt ab
			}
			nextID++
			v := &vm{workload: a.Workload, prof: p, zone: s.Zones[a.Zone], bootLeft: float64(p.BootS) / 3600,
				inputLeft: float64(w.InputS) / 3600}
			v.VM = planner.VM{ID: "vm" + strconv.Itoa(nextID), Name: a.Name, Zone: a.Zone, Family: p.Family, Size: p.Size, State: "creating"}
			vms = append(vms, v)
		case planner.Start, planner.Stop, planner.Delete:
			for _, v := range vms {
				if v.ID != a.VMID || v.deleted {
					continue
				}
				switch a.Kind {
				case planner.Start:
					if v.State == "stopped" {
						v.State = "running"
						if v.bootLeft > 0 {
							v.State = "creating"
						}
					}
				case planner.Stop:
					v.State = "stopped"
				case planner.Delete:
					v.deleted = true
					if j := jobs[v.workload]; j != nil && !j.delivered && alive(vms, v.workload) == 0 {
						if j.w.RestartFromZero {
							j.completed, j.checkpointed = 0, 0
						} else {
							j.completed = j.checkpointed
						}
						j.sinceCkpt = 0
					}
				}
			}
		}
	}

	for now := 0.0; now < Hours-1e-9; now += cfg.StepH {
		// geplante Aktionen wirksam werden lassen
		rest := queue[:0]
		for _, p := range queue {
			if p.at <= now {
				apply(p.a, now)
			} else {
				rest = append(rest, p)
			}
		}
		queue = rest
		if now >= nextPlan {
			in := input(s, world, jobs, vms, now)
			for _, a := range planner.Plan(s, in, cfg.Params) {
				queue = append(queue, pending{at: now + cfg.LagH, a: a})
			}
			nextPlan = now + cfg.PlanEvery
		}
		step(s, world, jobs, vms, now, cfg.StepH, &shared)
	}

	res := Result{Jobs: map[string]JobResult{}, Disqualified: disq}
	ids := make([]string, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids) // feste Summationsreihenfolge, sonst weichen Punkte in der letzten Stelle ab
	var refSum, all float64
	for _, id := range ids {
		j := jobs[id]
		refSum += ref[j.w.ID]
		all += j.carbon
	}
	all += shared
	res.ElectricityG = all
	sCost := all / (refSum * 200)
	allOnTime := true
	for _, id := range ids {
		j := jobs[id]
		dl := s.Hours(j.w.Deadline)
		prog := j.atDeadline
		if prog < 0 {
			prog = j.completed
		}
		prog = math.Min(prog/j.w.RequiredWork, 1)
		onTime := j.delivered && j.deliveredAt <= dl
		if onTime {
			res.OnTime++
		} else {
			allOnTime = false
		}
		maxI := 20000 * ref[id] / refSum
		li := j.carbon / (ref[id] * 200)
		res.Score += maxI * prog / (1 + 0.75*li + 0.25*sCost)
		res.Jobs[id] = JobResult{Delivered: j.delivered, DeliveredAt: j.deliveredAt, Progress: prog, CarbonG: j.carbon}
	}
	if allOnTime {
		res.Score += 5000
	}
	if disq {
		res.Score = 0
	}
	return res
}

func alive(vms []*vm, workload string) int {
	n := 0
	for _, v := range vms {
		if !v.deleted && v.workload == workload {
			n++
		}
	}
	return n
}

func fits(s *spec.Spec, vms []*vm, p spec.Profile) bool {
	n, cpu := 1, p.CPUCores
	gpu := map[string]int{p.Family: p.GPUCount}
	for _, v := range vms {
		if v.deleted {
			continue
		}
		n++
		cpu += v.prof.CPUCores
		gpu[v.prof.Family] += v.prof.GPUCount
	}
	if n > s.MaxVMs || cpu > s.MaxVCPUs {
		return false
	}
	return p.GPUCount == 0 || gpu[p.Family] <= s.GPUQuota[p.Family]
}

func input(s *spec.Spec, world *World, jobs map[string]*job, vms []*vm, now float64) planner.Input {
	in := planner.Input{Now: now, Jobs: map[string]planner.Job{}, Forecast: map[string][]float64{}}
	byJob := map[string][]planner.VM{}
	for _, v := range vms {
		if !v.deleted {
			byJob[v.workload] = append(byJob[v.workload], v.VM)
		}
	}
	for id, j := range jobs {
		in.Jobs[id] = planner.Job{Completed: j.completed, Delivered: j.delivered, VMs: byJob[id]}
	}
	for area := range world.Actual {
		in.Forecast[area] = world.Forecast(area, int(now))
	}
	return in
}

func step(s *spec.Spec, world *World, jobs map[string]*job, vms []*vm, now, dt float64, shared *float64) {
	computing := map[string][]*vm{}
	for _, v := range vms {
		if v.deleted || v.State == "stopped" {
			continue
		}
		j := jobs[v.workload]
		watts := v.prof.IdleW
		switch {
		case v.bootLeft > 0:
			watts = v.prof.ActiveW
			v.bootLeft -= dt
			if v.bootLeft <= 0 {
				v.State = "running"
			}
		case j == nil || j.delivered:
		case v.inputLeft > 0:
			watts = v.prof.ActiveW
			v.inputLeft -= dt
		case now < s.Hours(j.w.Release):
		case j.completed < j.w.RequiredWork:
			watts = v.prof.ActiveW
			computing[v.workload] = append(computing[v.workload], v)
		}
		g := watts / 1000 * dt * v.zone.PUE * world.Intensity(v.zone.Area, now)
		if j != nil {
			j.carbon += g
		} else {
			*shared += g
		}
	}
	ids := make([]string, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		j := jobs[id]
		if ws := computing[id]; len(ws) > 0 {
			rate := 0.0
			for _, v := range ws {
				rate += spec.RatePerHour(j.w, v.prof, 1)
			}
			rate /= 1 + j.w.SyncOverhead*float64(len(ws)-1)
			j.completed = math.Min(j.completed+rate*dt, j.w.RequiredWork)
			j.sinceCkpt += dt
			if ck := float64(j.w.CheckpointS) / 3600; ck > 0 && j.sinceCkpt >= ck {
				j.checkpointed, j.sinceCkpt = j.completed, 0
			}
		}
		if j.atDeadline < 0 && now+dt >= s.Hours(j.w.Deadline) {
			j.atDeadline = j.completed
		}
		if j.delivered || j.completed < j.w.RequiredWork {
			continue
		}
		j.checkpointed = j.completed
		if j.outputLeft < 0 {
			j.outputLeft = float64(j.w.OutputS) / 3600
		}
		for _, v := range vms {
			if !v.deleted && v.workload == id && v.State == "running" {
				j.outputLeft -= dt
				break
			}
		}
		if j.outputLeft <= 0 {
			j.delivered, j.deliveredAt = true, now+dt
		}
	}
}
