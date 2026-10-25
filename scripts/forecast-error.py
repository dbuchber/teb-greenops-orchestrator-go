#!/usr/bin/env python3
"""Prognosefehler eines Live-Laufs je Vorlauf: Orchestrator-Log (Zeilen "prognose") gegen Ist-Werte.

    ssh hetzner-wikidata docker logs greenops-<id> 2>&1 | scripts/forecast-error.py runs/<id>

Ist-Werte stammen aus runs/<id>/hourly_impact.csv (usage_gco2e / energy_kwh je Zone und Stunde), also nur für
Zonen und Stunden, in denen eine eigene VM lief. Ausgegeben werden nur Kennzahlen (Bias, Streuung je Vorlauf),
um das Fehlermodell des Simulators (internal/sim/grid.go) einzustellen, keine Werte des Szenarios.
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

actual = defaultdict(dict)  # Bereich -> Stunde -> g/kWh
for r in csv.DictReader(open(run / "hourly_impact.csv")):
    e = float(r["energy_kwh"])
    if e > 0:
        h = int((t(r["hour_start"]) - start).total_seconds() // 3600)
        actual[area[r["zone"]]][h] = float(r["usage_gco2e"]) / e

err = defaultdict(list)  # Vorlauf -> relative Fehler
for line in sys.stdin:
    try:
        e = json.loads(line)
    except ValueError:
        continue
    if e.get("msg") != "prognose":
        continue
    h0 = e["stunde"]
    for a, vals in e["werte"].items():
        for lead, f in enumerate(vals):
            act = actual.get(a, {}).get(h0 + lead)
            if act:
                err[lead].append(f / act - 1)

print("Vorlauf  n     Bias    Streuung")
for lead in sorted(err):
    xs = err[lead]
    m = sum(xs) / len(xs)
    sd = math.sqrt(sum((x - m) ** 2 for x in xs) / max(1, len(xs) - 1))
    print(f"{lead:5}  {len(xs):5}  {m:+7.3f}  {sd:7.3f}")
