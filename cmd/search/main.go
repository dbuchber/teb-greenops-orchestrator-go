// search tries random planner parameters and re-checks the best ones on fresh seeds.
//
//	go run ./cmd/search -n 300 -seeds 40 -top 8 -validate 200 -out search.csv
//
// Search and validation use separate seeds (1… and 10001…), so lucky hits in the search show up.
// Scoring is paired against the default parameters: Δ points per seed, mean ± 2 standard errors.
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/planner"
	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/sim"
	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/spec"
)

type cand struct {
	p              planner.Params
	desc           string
	d, se          float64 // Δ to the default setting (search)
	onTime         int
	vd, vse        float64 // Δ on the validation seeds
	vOnTime, vSeed int
	kg             float64
}

func sample(r *rand.Rand) (planner.Params, string) {
	p := planner.DefaultParams()
	pick := func(xs ...float64) float64 { return xs[r.IntN(len(xs))] }
	p.LifeCost = pick(0, 0.003, 0.01, 0.02, 0.03, 0.05)
	p.GPUWeight = int(pick(0, 2, 4, 8, 16, 32))
	p.Shrink = pick(0, 0, 0.1, 0.25, 0.5)
	p.MarginH = pick(0.5, 1, 2, 3)
	p.NoPauseH = pick(0.5, 1, 2, 4)
	p.MinCores = int(pick(0, 0, 16, 32, 40))
	p.EndSlices = int(pick(3, 6, 12))
	return p, describe(p)
}

func describe(p planner.Params) string {
	return fmt.Sprintf("life=%g gpu=%d shrink=%g margin=%g nopause=%g mincores=%d slices=%d",
		p.LifeCost, p.GPUWeight, p.Shrink, p.MarginH, p.NoPauseH, p.MinCores, p.EndSlices)
}

// eval runs all parameter sets on seeds first…first+n-1 in parallel.
func eval(s *spec.Spec, ref sim.Ref, ps []planner.Params, first, n int) [][]sim.Result {
	res := make([][]sim.Result, len(ps))
	for i := range res {
		res[i] = make([]sim.Result, n)
	}
	jobs := make(chan [2]int)
	var wg sync.WaitGroup
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range jobs {
				c := sim.DefaultConfig(uint64(first + k[1]))
				c.Params = ps[k[0]]
				res[k[0]][k[1]] = sim.Run(s, ref, c)
			}
		}()
	}
	for i := range ps {
		for k := 0; k < n; k++ {
			jobs <- [2]int{i, k}
		}
	}
	close(jobs)
	wg.Wait()
	return res
}

func paired(a, base []sim.Result) (d, se float64, onTime int, kg float64) {
	var xs []float64
	for k := range a {
		xs = append(xs, a[k].Score-base[k].Score)
		if a[k].OnTime == 15 && !a[k].Disqualified {
			onTime++
		}
		kg += a[k].ElectricityG / 1000 / float64(len(a))
	}
	m, ss := 0.0, 0.0
	for _, x := range xs {
		m += x
	}
	m /= float64(len(xs))
	for _, x := range xs {
		ss += (x - m) * (x - m)
	}
	return m, 2 * math.Sqrt(ss/float64(len(xs)-1)) / math.Sqrt(float64(len(xs))), onTime, kg
}

func main() {
	n := flag.Int("n", 200, "number of random candidates")
	seeds := flag.Int("seeds", 40, "seeds per candidate in the search")
	top := flag.Int("top", 8, "this many best candidates are re-checked")
	validate := flag.Int("validate", 200, "validation seeds per best candidate")
	out := flag.String("out", "search.csv", "result file")
	catalog := flag.String("catalog", "challenge", "catalog directory")
	seed := flag.Uint64("seed", 1, "random seed for the candidates")
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
	r := rand.New(rand.NewPCG(*seed, 99))
	ps := []planner.Params{planner.DefaultParams()}
	cands := []*cand{{p: ps[0], desc: "Default " + describe(ps[0])}}
	for i := 0; i < *n; i++ {
		p, d := sample(r)
		ps = append(ps, p)
		cands = append(cands, &cand{p: p, desc: d})
	}
	res := eval(s, ref, ps, 1, *seeds)
	for i, c := range cands {
		c.d, c.se, c.onTime, c.kg = paired(res[i], res[0])
	}
	sorted := append([]*cand(nil), cands[1:]...)
	// reliability first, then points: a missed bonus costs 5000
	sort.SliceStable(sorted, func(a, b int) bool {
		if sorted[a].onTime != sorted[b].onTime {
			return sorted[a].onTime > sorted[b].onTime
		}
		return sorted[a].d > sorted[b].d
	})
	best := append([]*cand{cands[0]}, sorted[:min(*top, len(sorted))]...)
	vps := make([]planner.Params, len(best))
	for i, c := range best {
		vps[i] = c.p
	}
	vres := eval(s, ref, vps, 10001, *validate)
	for i, c := range best {
		c.vd, c.vse, c.vOnTime, c.kg = paired(vres[i], vres[0])
		c.vSeed = *validate
	}
	f, err := os.Create(*out)
	if err == nil {
		fmt.Fprintln(f, "rank,search_delta,search_2se,search_15of15,validate_delta,validate_2se,validate_15of15,kg,parameters")
		for i, c := range best {
			fmt.Fprintf(f, "%d,%.1f,%.1f,%d,%.1f,%.1f,%d,%.3f,%q\n", i, c.d, c.se, c.onTime, c.vd, c.vse, c.vOnTime, c.kg, c.desc)
		}
		f.Close()
	}
	fmt.Printf("%d candidates × %d seeds, best %d × %d fresh seeds\n\n", *n, *seeds, len(best)-1, *validate)
	fmt.Printf("%-4s %16s %8s %16s %8s %7s  %s\n", "Rank", "Δ search", "15/15", "Δ validate", "15/15", "kg", "Parameters")
	for i, c := range best {
		fmt.Printf("%-4d %+7.1f ± %-6.1f %4d/%-3d %+7.1f ± %-6.1f %4d/%-3d %7.2f  %s\n", i, c.d, c.se, c.onTime, *seeds,
			c.vd, c.vse, c.vOnTime, *validate, c.kg, strings.TrimPrefix(c.desc, "Default "))
	}
}
