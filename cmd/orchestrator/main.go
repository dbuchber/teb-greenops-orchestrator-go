// Orchestrator for the Exoscale GreenOps Challenge (TEB AI Hackathon 2026).
//
// Configuration only via environment variables (official rerun) or the downloaded CLI file:
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

// account takes the environment (official rerun), otherwise the downloaded CLI configuration.
func account(path string) (api.Account, error) {
	env := api.Account{Name: "environment", Key: os.Getenv("EXOSCALE_API_KEY"), Secret: os.Getenv("EXOSCALE_API_SECRET"),
		Endpoint: os.Getenv("GREENOPS_CLOUD_ENDPOINT")}
	if env.Key != "" && env.Secret != "" && env.Endpoint != "" {
		return env, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return env, fmt.Errorf("no credentials: set EXOSCALE_API_KEY/SECRET and GREENOPS_CLOUD_ENDPOINT or provide %s (%w)", path, err)
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
	return env, fmt.Errorf("%s: default account missing", path)
}

func run() error {
	configPath := flag.String("config", "greenops-exo.json", "downloaded CLI configuration (otherwise environment)")
	catalog := flag.String("catalog", "challenge", "directory with catalog.json and workloads.json")
	dry := flag.Bool("dry", false, "read only and print the first decision; creates nothing")
	tick := flag.Duration("tick", 250*time.Millisecond, "interval between status polls")
	global := flag.Bool("global", true, "global quota planning (planner/global.go); -global=false: old planner")
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
