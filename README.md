# GreenOps-Orchestrator (Go)

Grundgerüst eines CO₂-bewussten Orchestrators für die
[Exoscale GreenOps Challenge](https://github.com/exogmi/greenops-challenge-starter-kit) (TEB AI Hackathon,
23.–25.10.2026). Er steuert die 15 Workloads über die Exoscale-kompatible API eines Simulationslaufs und
nutzt nur die Strom-Prognosen dieses Laufs.

Challenge-Daten (`challenge/`) stammen unverändert aus dem Starter-Kit, Commit `a1dd55e`.

## Aufbau

| Paket | Aufgabe |
|---|---|
| `internal/spec` | Katalog laden, Legalität jeder Platzierung (Residency, Länder, Profile, GPU-Speicher), Rechenzeiten |
| `internal/planner` | reine Entscheidungsfunktion `Plan(spec, input, params) → []Action`, ohne Uhr und Netzwerk |
| `internal/api` | Cloud (egoscale v3), Work-Status (EXO2-HMAC), Grid; ein gemeinsamer Keep-Alive-Transport |
| `internal/controller` | Schleife: Status → Prognosen (zur neuen Stunde, parallel) → planen → Aktionen parallel |
| `cmd/orchestrator` | Einstieg, Konfiguration aus Umgebung oder `greenops-exo.json` |

Geschwindigkeit: Bei 720× ist 1 s echte Zeit = 12 simulierte Minuten. Deshalb laufen Prognosen und
VM-Aktionen parallel, Verbindungen werden wiederverwendet, und die Zonen-Erkennung passiert vor dem ersten
Create (solange der Lauf READY ist, läuft die Uhr noch nicht).

## Strategie im Grundgerüst

1. **Wo:** legale (Zone, Profil)-Option mit den geringsten erwarteten Emissionen:
   Wirkleistung × PUE × Rechenstunden × Mittel der günstigsten Prognosestunden.
2. **Wann:** Eine VM rechnet in den günstigsten *k* Stunden bis zur Deadline (*k* = nötige Stunden) und wird
   in den anderen gestoppt. Bei weniger als 2 h Spielraum läuft sie durch.
3. **Sicherheit:** Legalität wird zweimal geprüft (Planer und direkt vor dem Create); 2 h Puffer vor jeder
   Deadline.
4. **Aufräumen:** VMs werden gelöscht, sobald `final_output_delivered` gilt.

Noch offen (Ausbau): mehrere Worker je Job, Kontingent-Reservierung für spätere Jobs, Parken (Löschen und
Neuanlegen) flexibler Jobs wie w-04/w-05/w-13/w-14, Vorab-Anlegen vor dem Release, Prognosekorrektur aus den
eigenen veröffentlichten Beobachtungen, Schutz gegen hängende API-Aufrufe (Uhr-Hochrechnung, Safe Mode).

## Bauen und testen

```sh
go test -race ./...
go build -o orchestrator ./cmd/orchestrator
```

## Ausführen

Zugangsdaten nie committen (`greenops-exo*.json` und `*.env` sind ignoriert).

```sh
export GREENOPS_GRID_ENDPOINT='…/runs/<id>/grid/v4' GREENOPS_GRID_SECRET='…'
./orchestrator -config greenops-exo.json -dry   # nur lesen, erste Entscheidung ausgeben, Uhr startet nicht
./orchestrator -config greenops-exo.json        # Lauf steuern
```

## Container (offizieller Rerun)

Läuft unter den Jury-Grenzen: 2 CPU, 1 GiB, Root-Dateisystem nur lesbar, nur `/tmp` beschreibbar, ohne
Root-Rechte. Konfiguration nur über Umgebungsvariablen.

```sh
docker build -t greenops-orchestrator .
docker run --rm --read-only --tmpfs /tmp --cpus 2 --memory 1g --env-file run.env greenops-orchestrator
```

`run.env`: `EXOSCALE_API_KEY`, `EXOSCALE_API_SECRET`, `GREENOPS_CLOUD_ENDPOINT`, `GREENOPS_WORK_ENDPOINT`,
`GREENOPS_GRID_ENDPOINT`, `GREENOPS_GRID_SECRET`. Vor der Abgabe die Basis-Images im `Dockerfile` per Digest pinnen.

## Regeln, die der Code einhält

- Kein Create in einem Land außerhalb der Residency (Versuch = Disqualifikation).
- Strom-Daten nur von den Grid-Endpunkten des gesteuerten Laufs, nur bereits veröffentlichte Werte.
- Keine Zeitreihen oder Zeitpläne aus anderen Läufen im Code oder in Dateien.
