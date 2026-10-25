// Package spec loads the public challenge catalog (zones, profiles, workloads, quotas) and checks the
// legality of a placement. Everything here is pure and network-free so it can run before every Create.
package spec

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

type Zone struct {
	Slug    string
	Country string
	Area    string // power area for the grid API (AT, BG, CH, ...)
	EU      bool
	PUE     float64
}

type Profile struct {
	ID         string
	Family     string
	Size       string
	CPUCores   int
	GPUCount   int
	GPUMemGiB  int
	ActiveW    float64
	IdleW      float64
	Throughput float64 // per hour (vcpu_hour) or per second (tflop, i.e. TFLOP/s)
	WorkUnit   string
	BootS      int
	Zones      []string
}

type Workload struct {
	ID              string
	Kind            string
	RequiredWork    float64
	WorkUnit        string
	Release         time.Time
	Deadline        time.Time
	Residency       string // ANYWHERE, EU or country code
	Allowed         []string
	Forbidden       []string
	AllowedProfiles []string
	AllowedFamilies []string
	MinGPUMemGiB    int
	MaxWorkers      int
	SyncOverhead    float64
	InputS          int
	OutputS         int
	CheckpointS     int
	RestartFromZero bool
}

type Spec struct {
	ScenarioID string
	Zones      map[string]Zone
	Profiles   []Profile
	Workloads  []Workload
	MaxVMs     int
	MaxVCPUs   int
	GPUQuota   map[string]int // cards per GPU family
	T0         time.Time      // start of the 120 h window
}

// Option is a legal combination of zone and profile for a workload.
type Option struct {
	Zone    Zone
	Profile Profile
}

func num(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		panic(fmt.Sprintf("spec: not a number %q", s))
	}
	return v
}

// Load reads catalog.json and workloads.json from dir.
func Load(dir string) (*Spec, error) {
	var cat struct {
		ScenarioID string `json:"scenario_id"`
		Zones      []struct {
			Slug, Country string
			Area          string `json:"electricity_area"`
			EU            bool   `json:"eu_member"`
			PUE           string `json:"pue"`
		} `json:"zones"`
		Profiles []struct {
			ID, Family, Size string
			CPUCores         int    `json:"cpu_cores"`
			GPUCount         int    `json:"gpu_count"`
			GPUMemGiB        int    `json:"gpu_memory_gib"`
			ActiveW          string `json:"active_watts"`
			IdleW            string `json:"idle_watts"`
			Throughput       string `json:"throughput"`
			WorkUnit         string `json:"work_unit"`
			BootS            int    `json:"boot_seconds"`
			Zones            []string
		} `json:"profiles"`
		MaxVMs   int            `json:"max_vms"`
		MaxVCPUs int            `json:"max_vcpus"`
		GPUQuota map[string]int `json:"gpu_family_quotas"`
	}
	var wls []struct {
		ID, Kind, Residency string
		RequiredWork        string   `json:"required_work"`
		WorkUnit            string   `json:"work_unit"`
		Release             string   `json:"release_at"`
		Deadline            string   `json:"deadline_at"`
		Allowed             []string `json:"allowed_countries"`
		Forbidden           []string `json:"forbidden_countries"`
		AllowedProfiles     []string `json:"allowed_profile_ids"`
		AllowedFamilies     []string `json:"allowed_families"`
		MinGPUMemGiB        int      `json:"minimum_gpu_memory_gib"`
		MaxWorkers          int      `json:"max_workers"`
		SyncOverhead        string   `json:"sync_overhead"`
		InputS              int      `json:"input_seconds"`
		OutputS             int      `json:"output_seconds"`
		CheckpointS         int      `json:"checkpoint_seconds"`
		RestartFromZero     bool     `json:"restart_from_zero"`
	}
	if err := readJSON(filepath.Join(dir, "catalog.json"), &cat); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, "workloads.json"), &wls); err != nil {
		return nil, err
	}
	s := &Spec{ScenarioID: cat.ScenarioID, Zones: map[string]Zone{}, MaxVMs: cat.MaxVMs, MaxVCPUs: cat.MaxVCPUs, GPUQuota: cat.GPUQuota}
	for _, z := range cat.Zones {
		s.Zones[z.Slug] = Zone{Slug: z.Slug, Country: z.Country, Area: z.Area, EU: z.EU, PUE: num(z.PUE)}
	}
	for _, p := range cat.Profiles {
		s.Profiles = append(s.Profiles, Profile{ID: p.ID, Family: p.Family, Size: p.Size, CPUCores: p.CPUCores,
			GPUCount: p.GPUCount, GPUMemGiB: p.GPUMemGiB, ActiveW: num(p.ActiveW), IdleW: num(p.IdleW),
			Throughput: num(p.Throughput), WorkUnit: p.WorkUnit, BootS: p.BootS, Zones: p.Zones})
	}
	var end time.Time
	for _, w := range wls {
		rel, err := time.Parse(time.RFC3339, w.Release)
		if err != nil {
			return nil, err
		}
		dl, err := time.Parse(time.RFC3339, w.Deadline)
		if err != nil {
			return nil, err
		}
		sync := 0.0
		if w.SyncOverhead != "" {
			sync = num(w.SyncOverhead)
		}
		s.Workloads = append(s.Workloads, Workload{ID: w.ID, Kind: w.Kind, RequiredWork: num(w.RequiredWork),
			WorkUnit: w.WorkUnit, Release: rel, Deadline: dl, Residency: w.Residency, Allowed: w.Allowed,
			Forbidden: w.Forbidden, AllowedProfiles: w.AllowedProfiles, AllowedFamilies: w.AllowedFamilies,
			MinGPUMemGiB: w.MinGPUMemGiB, MaxWorkers: w.MaxWorkers, SyncOverhead: sync, InputS: w.InputS,
			OutputS: w.OutputS, CheckpointS: w.CheckpointS, RestartFromZero: w.RestartFromZero})
		if dl.After(end) {
			end = dl
		}
	}
	s.T0 = end.Add(-120 * time.Hour)
	return s, nil
}

func readJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// Legal checks a placement. An error here must never lead to a Create: the mere attempt in a forbidden
// country disqualifies the run.
func (s *Spec) Legal(w Workload, zoneSlug string, p Profile) error {
	z, ok := s.Zones[zoneSlug]
	if !ok {
		return fmt.Errorf("unknown zone %s", zoneSlug)
	}
	if !slices.Contains(p.Zones, zoneSlug) {
		return fmt.Errorf("profile %s.%s not in %s", p.Family, p.Size, zoneSlug)
	}
	switch {
	case w.Residency == "ANYWHERE":
	case w.Residency == "EU":
		if !z.EU {
			return fmt.Errorf("%s: EU residency, %s is not EU", w.ID, z.Country)
		}
	default:
		if z.Country != w.Residency {
			return fmt.Errorf("%s: only %s, not %s", w.ID, w.Residency, z.Country)
		}
	}
	if len(w.Allowed) > 0 && !slices.Contains(w.Allowed, z.Country) {
		return fmt.Errorf("%s: %s not allowed", w.ID, z.Country)
	}
	if slices.Contains(w.Forbidden, z.Country) {
		return fmt.Errorf("%s: %s forbidden", w.ID, z.Country)
	}
	if len(w.AllowedProfiles) > 0 && !slices.Contains(w.AllowedProfiles, p.ID) {
		return fmt.Errorf("%s: profile %s not allowed", w.ID, p.ID)
	}
	if len(w.AllowedFamilies) > 0 && !slices.Contains(w.AllowedFamilies, p.Family) {
		return fmt.Errorf("%s: family %s not allowed", w.ID, p.Family)
	}
	if w.Kind == "gpu" && p.GPUCount == 0 {
		return fmt.Errorf("%s: needs GPU", w.ID)
	}
	if w.MinGPUMemGiB > 0 && p.GPUMemGiB < w.MinGPUMemGiB {
		return fmt.Errorf("%s: %d GiB GPU memory < %d", w.ID, p.GPUMemGiB, w.MinGPUMemGiB)
	}
	if p.WorkUnit != w.WorkUnit {
		return fmt.Errorf("%s: unit %s does not match %s", w.ID, p.WorkUnit, w.WorkUnit)
	}
	return nil
}

// Options returns all legal (zone, profile) pairs of a workload.
func (s *Spec) Options(w Workload) []Option {
	var out []Option
	for _, p := range s.Profiles {
		for _, z := range p.Zones {
			if s.Legal(w, z, p) == nil {
				out = append(out, Option{Zone: s.Zones[z], Profile: p})
			}
		}
	}
	return out
}

// RatePerHour is the work per hour with "workers" active VMs (sync overhead per additional worker).
func RatePerHour(w Workload, p Profile, workers int) float64 {
	r := p.Throughput
	if p.WorkUnit == "tflop" {
		r *= 3600
	}
	return r * float64(workers) / (1 + w.SyncOverhead*float64(workers-1))
}

// ComputeHours is the pure compute time for "work" units.
func ComputeHours(w Workload, p Profile, workers int, work float64) float64 {
	return work / RatePerHour(w, p, workers)
}

// Hours converts a time to hours since T0.
func (s *Spec) Hours(t time.Time) float64 { return t.Sub(s.T0).Hours() }

func (s *Spec) Profile(family, size string) (Profile, bool) {
	for _, p := range s.Profiles {
		if p.Family == family && p.Size == size {
			return p, true
		}
	}
	return Profile{}, false
}

func (s *Spec) Workload(id string) (Workload, bool) {
	for _, w := range s.Workloads {
		if w.ID == id {
			return w, true
		}
	}
	return Workload{}, false
}

// Ceil rounds hours up to whole hours (at least 1).
func Ceil(h float64) int { return max(1, int(math.Ceil(h-1e-9))) }
