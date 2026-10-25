// Package sim ist ein lokaler Simulator der Challenge für Monte-Carlo-Vergleiche von Planer-Varianten.
//
// Die Strom-Welt ist rein synthetisch (erfundene Niveaus je Land, Tagesgang, mehrtägige Wetterphasen,
// Prognosefehler, die mit dem Vorlauf wachsen). Sie nutzt bewusst keine echten oder historischen Daten des
// Szenarios: das verbieten die Regeln. Ergebnisse taugen zum Vergleichen von Varianten auf denselben Seeds,
// nicht als Vorhersage der Live-Punkte.
package sim

import (
	"math"
	"math/rand/v2"
	"sort"
)

const Hours = 120

// Grundniveaus je Strombereich (g/kWh), frei erfunden; je Seed zufällig skaliert, damit sich die Rangfolge ändert.
var baseLevel = map[string]float64{"AT": 130, "BG": 380, "CH": 60, "DE": 330, "ES": 190, "FR": 70, "HR": 220, "IT": 290}

type World struct {
	Actual   map[string][]float64   // je Bereich: Ist-Wert je Stunde (Hours+24)
	forecast map[string][][]float64 // je Bereich und Ausgabestunde: 24 Werte ab dieser Stunde
}

// NewWorld erzeugt eine Welt aus dem Seed; gleicher Seed, gleiche Welt (für gepaarte Vergleiche).
func NewWorld(seed uint64) *World {
	w := &World{Actual: map[string][]float64{}, forecast: map[string][][]float64{}}
	areas := make([]string, 0, len(baseLevel))
	for a := range baseLevel {
		areas = append(areas, a)
	}
	sort.Strings(areas) // feste Reihenfolge, sonst hängt die Welt von der Map-Iteration ab
	for i, a := range areas {
		r := rand.New(rand.NewPCG(seed, uint64(i)*7919+1))
		level := baseLevel[a] * (0.6 + 0.8*r.Float64())
		// Tagesgang: Minimum um 13 Uhr (Solar). Spanne so gewählt, dass die Kennzahlen (TestWorldStats) zu den
		// Live-Läufen passen: Tagesamplitude ~0,35, CV ~0,26, günstigste Hälfte ~0,79 des Mittels.
		// Prognosefehler wie live: glatter Verlauf, Ist-Werte weichen stundenweise korreliert ab.
		amp := 0.15 + 0.3*r.Float64()
		weather := 0.0
		n := Hours + 24
		smooth := make([]float64, n) // was die Prognose kennt
		act := make([]float64, n)
		dev := 0.0
		for h := 0; h < n; h++ {
			weather = 0.97*weather + 0.03*r.NormFloat64() // mehrtägige Wetterphasen
			daily := -amp * math.Cos(2*math.Pi*float64((h%24)-13)/24)
			smooth[h] = math.Max(10, level*(1+daily+weather))
			// Abweichung der Ist-Werte vom glatten Verlauf: live gemessen ~13 % Streuung, über Stunden
			// korreliert (1-h-Autokorrelation 0,2–0,76), unabhängig vom Vorlauf (scripts/forecast-error.py)
			dev = 0.6*dev + 0.104*r.NormFloat64()
			act[h] = math.Max(10, smooth[h]*(1+dev))
		}
		w.Actual[a] = act
		// Prognose: der glatte Verlauf mit kleinen Revisionen (live: 5 % zwischen Vorlauf 23 und 0)
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

// Forecast ist die zur Stunde “hour“ veröffentlichte 24-h-Prognose (Index 0 = diese Stunde).
func (w *World) Forecast(area string, hour int) []float64 {
	hour = min(max(hour, 0), Hours-1)
	return w.forecast[area][hour]
}

func (w *World) Intensity(area string, t float64) float64 {
	h := min(max(int(t), 0), Hours+23)
	return w.Actual[area][h]
}
