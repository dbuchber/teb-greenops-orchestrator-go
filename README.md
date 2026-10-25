# GreenOps Orchestrator (Go)

A carbon-aware orchestrator for the
[Exoscale GreenOps Challenge](https://github.com/exogmi/greenops-challenge-starter-kit) (TEB AI Hackathon,
23–25 Oct 2026). It controls the 15 workloads through the Exoscale-compatible API of a simulation run and
uses only the power forecasts of that run. It started as a skeleton; the sections below describe the current
state.

Challenge data (`challenge/`) is taken unchanged from the starter kit, commit `a1dd55e`.

## Layout

| Package | Purpose |
|---|---|
| `internal/spec` | load the catalog, legality of every placement (residency, countries, profiles, GPU memory), compute times |
| `internal/planner` | pure decision function `Plan(spec, input, params) → []Action`, no clock, no network |
| `internal/api` | cloud (egoscale v3), work status (EXO2-HMAC), grid; one shared keep-alive transport |
| `internal/controller` | loop: status → forecasts (at each new hour, in parallel) → plan → actions in parallel |
| `internal/sim` | local Monte Carlo simulator of the challenge with a synthetic power world |
| `cmd/orchestrator` | entry point, configuration from the environment or `greenops-exo.json` |
| `cmd/sim` | compares planner variants on the same seeds (paired) |
| `cmd/search` | random parameter search, best candidates re-checked on fresh seeds (results in `docs/search/`) |

Speed: at 720× 1 s of real time = 12 simulated minutes. So forecasts and VM actions run in parallel,
connections are reused, and zone discovery happens before the first Create (while the run is READY the clock
is not ticking yet).

## Strategy

The default is global quota planning (`internal/planner/global.go`, `-global=true`):

1. **Lifetime per job:** a VM holds quota from Create to deletion, even when stopped. Every job gets a lifetime
   [start, end) and computes within it in its cleanest forecast hours. Cost = expected emissions + a small
   charge per occupied vCPU hour (a GPU card counts as several vCPU).
2. **All jobs together:** existing VMs first, then new jobs greedily against an hourly quota occupancy. The
   planner tries 48 job orders and every lifetime end and keeps the cheapest plan that places all jobs; it
   re-plans at every new hour. Beyond the 24 h forecast the daily pattern is continued.
3. **Safety:** legality is checked twice (planner and right before the Create); 2 h buffer before every
   deadline; with less than 2 h of slack a VM runs straight through; if a released job finds no quota, paused
   VMs resume to free it.
4. **Cleanup:** VMs are deleted as soon as `final_output_delivered` holds.

The original local planner (`-global=false`) picks the cheapest legal (zone, profile) option per job and runs
in the cheapest *k* hours until the deadline. Parking (delete and re-create) and just-in-time creation exist
as switches but are off by default.

Still open: several workers per job, forecast correction from our own published observations, protection
against hanging API calls (clock extrapolation, safe mode).

## Simulator and tools

- `go run ./cmd/sim -seeds 200`: paired Monte Carlo comparison of planner variants. The simulator is
  calibrated on live runs (power per phase, forecast error, daily amplitude) but uses no scenario data; it
  compares variants, it does not predict live points.
- `go run ./cmd/search -n 300 -seeds 40 -top 8 -validate 200`: random parameter search.
- `scripts/live-run.sh`: starts a run on the server and summarises the logs; `--new` creates the run in the
  portal via `scripts/portal` (Playwright), `--watch` starts every newly downloaded run.
- `scripts/portal/portal.mjs export`: exports all runs to `runs/` as JSON and CSV.
- `scripts/forecast-error.py`: forecast error per lead time from the orchestrator log against the actual values.

## Results

Best live result 21,992 points (run `d5067ed73f`, v4); v6 scored 21,982 live. In the simulator v6
(48 job orders, every lifetime end) is +44 points ahead of v5.

## Build and test

```sh
go test -race ./...
go build -o orchestrator ./cmd/orchestrator
```

## Run

Never commit credentials (`greenops-exo*.json` and `*.env` are ignored).

```sh
export GREENOPS_GRID_ENDPOINT='…/runs/<id>/grid/v4' GREENOPS_GRID_SECRET='…'
./orchestrator -config greenops-exo.json -dry   # read only, print the first decision, clock does not start
./orchestrator -config greenops-exo.json        # control the run
```

## Container (official rerun)

Runs within the jury limits: 2 CPU, 1 GiB, read-only root filesystem, only `/tmp` writable, no root
privileges. Configuration only via environment variables.

```sh
docker build -t greenops-orchestrator .
docker run --rm --read-only --tmpfs /tmp --cpus 2 --memory 1g --env-file run.env greenops-orchestrator
```

`run.env`: `EXOSCALE_API_KEY`, `EXOSCALE_API_SECRET`, `GREENOPS_CLOUD_ENDPOINT`, `GREENOPS_WORK_ENDPOINT`,
`GREENOPS_GRID_ENDPOINT`, `GREENOPS_GRID_SECRET`. Before submission pin the base images in the `Dockerfile` by digest.

## Rules the code follows

- No Create in a country outside the residency (an attempt = disqualification).
- Power data only from the grid endpoints of the controlled run, only values already published.
- No time series or schedules from other runs in the code or in files.
