// Orchestrator für die Exoscale GreenOps Challenge (TEB AI Hackathon 2026).
//
// Konfiguration nur über Umgebungsvariablen (offizieller Rerun) oder die heruntergeladene CLI-Datei:
//
//	EXOSCALE_API_KEY, EXOSCALE_API_SECRET, GREENOPS_CLOUD_ENDPOINT, GREENOPS_WORK_ENDPOINT (optional),
//	GREENOPS_GRID_ENDPOINT, GREENOPS_GRID_SECRET
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/api"
	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/controller"
	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/planner"
	"github.com/dbuchber/teb-greenops-orchestrator-go/internal/spec"
)

// account nimmt die Umgebung (offizieller Rerun), sonst die heruntergeladene CLI-Konfiguration.
func account(path string) (api.Account, error) {
	env := api.Account{Name: "environment", Key: os.Getenv("EXOSCALE_API_KEY"), Secret: os.Getenv("EXOSCALE_API_SECRET"),
		Endpoint: os.Getenv("GREENOPS_CLOUD_ENDPOINT")}
	if env.Key != "" && env.Secret != "" && env.Endpoint != "" {
		return env, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return env, fmt.Errorf("keine Zugangsdaten: EXOSCALE_API_KEY/SECRET und GREENOPS_CLOUD_ENDPOINT setzen oder %s ablegen (%w)", path, err)
	}
	var cfg struct {
		DefaultAccount string `json:"defaultaccount"`
		Accounts       []api.Account
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return env, err
	}
	for _, a := range cfg.Accounts {
		if a.Name == cfg.DefaultAccount {
			return a, nil
		}
	}
	return env, fmt.Errorf("%s: Standard-Account fehlt", path)
}

func run() error {
	configPath := flag.String("config", "greenops-exo.json", "heruntergeladene CLI-Konfiguration (sonst Umgebung)")
	catalog := flag.String("catalog", "challenge", "Ordner mit catalog.json und workloads.json")
	dry := flag.Bool("dry", false, "nur lesen und die erste Entscheidung ausgeben; legt nichts an")
	tick := flag.Duration("tick", 250*time.Millisecond, "Abstand der Status-Abfragen")
	global := flag.Bool("global", true, "globale Kontingentplanung (planner/global.go); -global=false: alter Planer")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	s, err := spec.Load(*catalog)
	if err != nil {
		return err
	}
	acc, err := account(*configPath)
	if err != nil {
		return err
	}
	work, err := api.NewWork(acc, os.Getenv("GREENOPS_WORK_ENDPOINT"))
	if err != nil {
		return err
	}
	grid, err := api.NewGrid(os.Getenv("GREENOPS_GRID_ENDPOINT"), os.Getenv("GREENOPS_GRID_SECRET"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	params := planner.DefaultParams()
	params.Global = *global
	if *global {
		params.Cache = &planner.GlobalCache{}
	}
	c := &controller.Controller{Spec: s, Work: work, Grid: grid, Cloud: api.NewCloud(acc), Params: params,
		Log: log, Dry: *dry, Tick: *tick}
	log.Info("start", "scenario", s.ScenarioID, "dry", *dry, "global", *global)
	return c.Run(ctx)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "orchestrator:", err)
		os.Exit(1)
	}
}
