// sim compares planner variants on the same synthetic seeds (paired Monte Carlo runs).
//
//	go run ./cmd/sim -seeds 200
//
// Results compare variants; they do not predict live points.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/sim"
	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/spec"
)

type variant struct {
	name string
	cfg  func(seed uint64) sim.Config
	mod  func(*spec.Spec) // optional: modified world, e.g. a higher quota cap
}

// variants: reference first; every other one changes exactly one assumption against the reference.
func variants() []variant {
	base := sim.DefaultConfig
	with := func(f func(*sim.Config)) func(uint64) sim.Config {
		return func(seed uint64) sim.Config { c := base(seed); f(&c); return c }
	}
	return []variant{
		{"reference", base, nil},
		{"old planner", with(func(c *sim.Config) { c.Params.Global = false }), nil},
		{"v5 (1 order, 6 ends)", with(func(c *sim.Config) { c.Params.Orders = 1; c.Params.AllEnds = false }), nil},
		{"GPU first", with(func(c *sim.Config) { c.Params.GPUFirst = true }), nil},
		{"GPU first, 1 order", with(func(c *sim.Config) { c.Params.GPUFirst = true; c.Params.Orders = 1 }), nil},
		{"v5 + GPU first", with(func(c *sim.Config) { c.Params.GPUFirst = true; c.Params.Orders = 1; c.Params.AllEnds = false }), nil},
		{"GPU first, 1 s lag", with(func(c *sim.Config) { c.Params.GPUFirst = true; c.LagH = 0.2 }), nil},
		{"vCPU quota ×2", base, func(s *spec.Spec) { s.MaxVCPUs *= 2 }},
	}
}

func main() {
	seeds := flag.Int("seeds", 100, "number of seeds per variant")
	catalog := flag.String("catalog", "challenge", "directory with catalog.json, workloads.json, rules.json")
	flag.Parse()
	s, err := spec.Load(*catalog)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ref, err := sim.LoadRef(*catalog)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	vs := variants()
	specs := make([]*spec.Spec, len(vs))
	for i, v := range vs {
		specs[i] = s
		if v.mod != nil {
			c, err := spec.Load(*catalog)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			v.mod(c)
			specs[i] = c
		}
	}
	res := make([][]sim.Result, len(vs))
	for i := range res {
		res[i] = make([]sim.Result, *seeds)
	}
	start := time.Now()
	var wg sync.WaitGroup
	jobs := make(chan [2]int)
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range jobs {
				res[k[0]][k[1]] = sim.Run(specs[k[0]], ref, vs[k[0]].cfg(uint64(k[1]+1)))
			}
		}()
	}
	for i := range vs {
		for k := 0; k < *seeds; k++ {
			jobs <- [2]int{i, k}
		}
	}
	close(jobs)
	wg.Wait()

	fmt.Printf("%d seeds per variant, %d runs in %s (synthetic power world, for comparison only)\n\n",
		*seeds, len(vs)**seeds, time.Since(start).Round(time.Millisecond))
	fmt.Printf("%-16s %10s %8s %8s %9s %8s %16s\n", "Variant", "Points", "sd", "P10", "15/15", "kg", "Δ to reference")
	for i, v := range vs {
		pts, kg := make([]float64, *seeds), 0.0
		full := 0
		var diff []float64
		for k, r := range res[i] {
			pts[k] = r.Score
			kg += r.ElectricityG / 1000
			if r.OnTime == 15 && !r.Disqualified {
				full++
			}
			diff = append(diff, r.Score-res[0][k].Score)
		}
		m, sd := meanSD(pts)
		sort.Float64s(pts)
		d, dsd := meanSD(diff)
		delta := "–"
		if i > 0 {
			delta = fmt.Sprintf("%+.1f ± %.1f", d, 2*dsd/math.Sqrt(float64(len(diff))))
		}
		fmt.Printf("%-16s %10.1f %8.1f %8.1f %5d/%-3d %8.2f %16s\n", v.name, m, sd, pts[len(pts)/10], full, *seeds,
			kg/float64(*seeds), delta)
	}
	fmt.Println("\nΔ: paired mean ± 2 standard errors; if the interval excludes 0, the difference is robust.")
}

func meanSD(x []float64) (float64, float64) {
	if len(x) == 0 {
		return 0, 0
	}
	m := 0.0
	for _, v := range x {
		m += v
	}
	m /= float64(len(x))
	ss := 0.0
	for _, v := range x {
		ss += (v - m) * (v - m)
	}
	if len(x) < 2 {
		return m, 0
	}
	return m, math.Sqrt(ss / float64(len(x)-1))
}
