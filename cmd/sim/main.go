// sim vergleicht Planer-Varianten auf denselben synthetischen Seeds (gepaarte Monte-Carlo-Läufe).
//
//	go run ./cmd/sim -seeds 200
//
// Ergebnisse sind Vergleiche zwischen Varianten, keine Vorhersage der Live-Punkte.
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
}

// variants: Referenz zuerst; jede weitere ändert genau eine Annahme gegenüber der Referenz.
func variants() []variant {
	base := sim.DefaultConfig
	with := func(f func(*sim.Config)) func(uint64) sim.Config {
		return func(seed uint64) sim.Config { c := base(seed); f(&c); return c }
	}
	return []variant{
		{"referenz", base},
		{"alter Planer", with(func(c *sim.Config) { c.Params.Global = false })},
		{"global, Muster 0,5", with(func(c *sim.Config) { c.Params.Global = true; c.Params.Shrink = 0.5 })},
		{"global, margin 4", with(func(c *sim.Config) { c.Params.Global = true; c.Params.MarginH = 4 })},
		{"global, 1 s Lag", with(func(c *sim.Config) { c.Params.Global = true; c.LagH = 0.2 })},
		{"720x, 1 s Lag", with(func(c *sim.Config) { c.LagH = 0.2 })},
	}
}

func main() {
	seeds := flag.Int("seeds", 100, "Anzahl Seeds je Variante")
	catalog := flag.String("catalog", "challenge", "Ordner mit catalog.json, workloads.json, rules.json")
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
				res[k[0]][k[1]] = sim.Run(s, ref, vs[k[0]].cfg(uint64(k[1]+1)))
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

	fmt.Printf("%d Seeds je Variante, %d Läufe in %s (synthetische Strom-Welt, nur zum Vergleichen)\n\n",
		*seeds, len(vs)**seeds, time.Since(start).Round(time.Millisecond))
	fmt.Printf("%-16s %10s %8s %8s %9s %8s %16s\n", "Variante", "Punkte", "sd", "P10", "15/15", "kg", "Δ zur Referenz")
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
	fmt.Println("\nΔ: gepaarter Mittelwert ± 2 Standardfehler; ohne Überlappung mit 0 ist der Unterschied belastbar.")
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
