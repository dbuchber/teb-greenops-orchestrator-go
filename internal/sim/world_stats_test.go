package sim

import (
	"math"
	"os"
	"sort"
	"testing"
)

// Statistics of the synthetic world for comparison with live runs: WORLD_STATS=1 go test ./internal/sim -run WorldStats -v
func TestWorldStats(t *testing.T) {
	if os.Getenv("WORLD_STATS") == "" {
		t.Skip("on request only")
	}
	var cv, half, amp float64
	n := 0
	for seed := uint64(1); seed <= 50; seed++ {
		w := NewWorld(seed)
		for _, v := range w.Actual {
			v = v[:Hours]
			m, ss := 0.0, 0.0
			for _, x := range v {
				m += x
			}
			m /= float64(len(v))
			for _, x := range v {
				ss += (x - m) * (x - m)
			}
			cv += math.Sqrt(ss/float64(len(v))) / m
			s := append([]float64(nil), v...)
			sort.Float64s(s)
			h := 0.0
			for _, x := range s[:len(s)/2] {
				h += x
			}
			half += h / float64(len(s)/2) / m
			var prof [24]float64
			for i, x := range v {
				prof[i%24] += x / m / 5
			}
			lo, hi := math.Inf(1), math.Inf(-1)
			for _, x := range prof {
				lo, hi = math.Min(lo, x), math.Max(hi, x)
			}
			amp += (hi - lo) / 2
			n++
		}
	}
	t.Logf("CV %.3f  cheapest 50%%/mean %.3f  daily amplitude %.3f", cv/float64(n), half/float64(n), amp/float64(n))
}
