#!/usr/bin/env python3
"""Forecast error of a live run per lead time: orchestrator log (lines "forecast") against actual values.

    ssh hetzner-wikidata docker logs greenops-<id> 2>&1 | scripts/forecast-error.py runs/<id>

Actual values come from runs/<id>/hourly_impact.csv (usage_gco2e / energy_kwh per zone and hour), so only for
zones and hours in which one of our VMs ran. Only statistics are printed (bias, spread per lead time) to tune
the simulator's error model (internal/sim/grid.go), no values of the scenario.
"""
import csv
import json
import math
import sys
from collections import defaultdict
from datetime import datetime
from pathlib import Path

run = Path(sys.argv[1])
t = lambda s: datetime.fromisoformat(s.replace("Z", "+00:00"))
cat = json.load(open(Path(__file__).resolve().parent.parent / "challenge" / "catalog.json"))
area = {z["slug"]: z["electricity_area"] for z in cat["zones"]}
start = t(json.load(open(run / "run.json"))["run"]["start_at"])

actual = defaultdict(dict)  # area -> hour -> g/kWh
for r in csv.DictReader(open(run / "hourly_impact.csv")):
    e = float(r["energy_kwh"])
    if e > 0:
        h = int((t(r["hour_start"]) - start).total_seconds() // 3600)
        actual[area[r["zone"]]][h] = float(r["usage_gco2e"]) / e

err = defaultdict(list)  # lead time -> relative errors
for line in sys.stdin:
    try:
        e = json.loads(line)
    except ValueError:
        continue
    if e.get("msg") != "forecast":
        continue
    h0 = e["hour"]
    for a, vals in e["values"].items():
        for lead, f in enumerate(vals):
            act = actual.get(a, {}).get(h0 + lead)
            if act:
                err[lead].append(f / act - 1)

print("lead     n     bias    spread")
for lead in sorted(err):
    xs = err[lead]
    m = sum(xs) / len(xs)
    sd = math.sqrt(sum((x - m) ** 2 for x in xs) / max(1, len(xs) - 1))
    print(f"{lead:5}  {len(xs):5}  {m:+7.3f}  {sd:7.3f}")
