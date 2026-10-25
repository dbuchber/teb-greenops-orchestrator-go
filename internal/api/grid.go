package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Point ist ein Stundenwert der CO₂-Intensität (gCO₂e/kWh).
type Point struct {
	Time   time.Time `json:"datetime"`
	GCO2kW float64   `json:"carbonIntensity"`
}

// Grid liest nur die Strom-Endpunkte des eigenen Laufs (Regel: keine fremden oder historischen Daten).
type Grid struct {
	base   string
	secret string
	http   *http.Client
}

func NewGrid(endpoint, secret string) (*Grid, error) {
	if endpoint == "" || secret == "" {
		return nil, fmt.Errorf("GREENOPS_GRID_ENDPOINT und GREENOPS_GRID_SECRET fehlen")
	}
	if !strings.Contains(endpoint, "/runs/") || !strings.HasSuffix(strings.TrimSuffix(endpoint, "/"), "/grid/v4") {
		return nil, fmt.Errorf("kein Grid-Endpunkt /runs/{id}/grid/v4: %s", endpoint)
	}
	return &Grid{base: strings.TrimSuffix(endpoint, "/"), secret: secret, http: newHTTP(10 * time.Second)}, nil
}

func (g *Grid) get(ctx context.Context, path string, q url.Values, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("auth-token", g.secret)
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return RateLimited{}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("grid %s HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// Forecast liefert die aktuelle 24-h-Prognose eines Strombereichs (AT, DE, ...).
func (g *Grid) Forecast(ctx context.Context, area string) ([]Point, error) {
	var out struct {
		Forecast []Point `json:"forecast"`
	}
	q := url.Values{"zone": {area}, "horizonHours": {"24"}, "temporalGranularity": {"hourly"}}
	err := g.get(ctx, "/carbon-intensity/forecast", q, &out)
	return out.Forecast, err
}

// Latest liefert den zuletzt veröffentlichten Ist-Wert.
func (g *Grid) Latest(ctx context.Context, area string) (Point, error) {
	var out Point
	err := g.get(ctx, "/carbon-intensity/latest", url.Values{"zone": {area}}, &out)
	return out, err
}

// Forecasts holt die Prognosen aller Bereiche parallel; fehlgeschlagene Bereiche fehlen im Ergebnis.
func (g *Grid) Forecasts(ctx context.Context, areas []string) (map[string][]Point, map[string]error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	out, errs := map[string][]Point{}, map[string]error{}
	for _, a := range areas {
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			pts, err := g.Forecast(ctx, a)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[a] = err
				return
			}
			out[a] = pts
		}(a)
	}
	wg.Wait()
	return out, errs
}
