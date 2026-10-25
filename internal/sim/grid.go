// Package sim is a local simulator of the challenge for Monte Carlo comparisons of planner variants.
//
// The power world is purely synthetic (made-up levels per country, daily cycle, multi-day weather phases,
// forecast errors that grow with lead time). It deliberately uses no real or historical data of the
// scenario: the rules forbid that. Results are good for comparing variants on the same seeds, not as a
// prediction of live points.
package sim

import (
	"math"
	"math/rand/v2"
	"sort"
)

const Hours = 120

// Base levels per power area (g/kWh), made up; randomly scaled per seed so the ranking changes.
var baseLevel = map[string]float64{"AT": 130, "BG": 380, "CH": 60, "DE": 330, "ES": 190, "FR": 70, "HR": 220, "IT": 290}

type World struct {
	Actual   map[string][]float64   // per area: actual value per hour (Hours+24)
	forecast map[string][][]float64 // per area and issue hour: 24 values from that hour
}

// NewWorld creates a world from the seed; same seed, same world (for paired comparisons).
func NewWorld(seed uint64) *World {
	w := &World{Actual: map[string][]float64{}, forecast: map[string][][]float64{}}
	areas := make([]string, 0, len(baseLevel))
	for a := range baseLevel {
		areas = append(areas, a)
	}
	sort.Strings(areas) // fixed order, otherwise the world depends on map iteration
	for i, a := range areas {
		r := rand.New(rand.NewPCG(seed, uint64(i)*7919+1))
		level := baseLevel[a] * (0.6 + 0.8*r.Float64())
		// Daily cycle: minimum at 13:00 (solar). Range chosen so the statistics (TestWorldStats) match the
		// live runs: daily amplitude ~0.35, CV ~0.26, cheapest half ~0.79 of the mean.
		// Forecast error as live: smooth curve, actual values deviate with hourly correlation.
		amp := 0.15 + 0.3*r.Float64()
		weather := 0.0
		n := Hours + 24
		smooth := make([]float64, n) // what the forecast knows
		act := make([]float64, n)
		dev := 0.0
		for h := 0; h < n; h++ {
			weather = 0.97*weather + 0.03*r.NormFloat64() // multi-day weather phases
			daily := -amp * math.Cos(2*math.Pi*float64((h%24)-13)/24)
			smooth[h] = math.Max(10, level*(1+daily+weather))
			// deviation of actual values from the smooth curve: measured live ~13 % spread, correlated over
			// hours (1 h autocorrelation 0.2–0.76), independent of lead time (scripts/forecast-error.py)
			dev = 0.6*dev + 0.104*r.NormFloat64()
			act[h] = math.Max(10, smooth[h]*(1+dev))
		}
		w.Actual[a] = act
		// forecast: the smooth curve with small revisions (live: 5 % between lead time 23 and 0)
		fixed := make([]float64, n)
		for h := range fixed {
			fixed[h] = 0.03 * r.NormFloat64()
		}
		fcs := make([][]float64, Hours)
		for h := 0; h < Hours; h++ {
			fc := make([]float64, 24)
			for l := 0; l < 24; l++ {
				e := fixed[h+l] + 0.04*float64(l)/23*r.NormFloat64()
				fc[l] = math.Max(5, smooth[h+l]*(1+e))
			}
			fcs[h] = fc
		}
		w.forecast[a] = fcs
	}
	return w
}

// Forecast is the 24 h forecast published at hour "hour" (index 0 = that hour).
func (w *World) Forecast(area string, hour int) []float64 {
	hour = min(max(hour, 0), Hours-1)
	return w.forecast[area][hour]
}

func (w *World) Intensity(area string, t float64) float64 {
	h := min(max(int(t), 0), Hours+23)
	return w.Actual[area][h]
}
