#!/usr/bin/env bash
# Startet den Orchestrator für einen neuen Lauf auf hetzner und wartet auf das Ende.
#
#	scripts/live-run.sh                         # neueste greenops-<id>.env aus ~/Downloads
#	scripts/live-run.sh ~/Downloads/greenops-85e472c9e6.env
#	IMAGE=greenops-orch-go:v3 scripts/live-run.sh
#
# Die Zugangsdaten werden nie ausgegeben: die Datei geht ungelesen per scp auf den Server, gezeigt
# werden nur Schlüsselnamen. Läufe, für die schon ein Container existiert, werden nicht neu gestartet.
set -euo pipefail

HOST=${HOST:-hetzner-wikidata}
DIR=${DIR:-/data/infra-kg-austria/work/greenops-orch-go}
IMAGE=${IMAGE:-greenops-orch-go:v2}
SSH=(ssh -o ClearAllForwardings=yes "$HOST")

env_file=${1:-$(ls -t ~/Downloads/greenops-*.env 2>/dev/null | head -1)}
[[ -f $env_file ]] || { echo "keine greenops-<id>.env gefunden" >&2; exit 1; }
id=$(basename "$env_file" .env)
id=${id#greenops-}
name=greenops-$id
echo "Lauf $id, Image $IMAGE"

if "${SSH[@]}" "docker ps -a --format '{{.Names}}' | grep -qx $name"; then
	echo "Container $name existiert schon; Ergebnis:" >&2
else
	scp -q -o ClearAllForwardings=yes "$env_file" "$HOST:$DIR/.local/$id.env"
	"${SSH[@]}" "cd $DIR/.local && chmod 600 $id.env &&
		sed -e 's/^export //' -e 's/=\"\(.*\)\"\$/=\1/' -e \"s/='\(.*\)'\$/=\1/\" $id.env > $id.docker.env &&
		chmod 600 $id.docker.env && grep -v '^#' $id.docker.env | cut -d= -f1 | tr '\n' ' '; echo"

	echo "Trockenlauf …"
	dry=$("${SSH[@]}" "timeout 60 docker run --rm --read-only --tmpfs /tmp --user 65534:65534 \
		--env-file $DIR/.local/$id.docker.env $IMAGE -dry 2>&1")
	echo "$dry" | grep -o '"state":"[A-Z_]*"\|"speed":[0-9]*\|"workloads":[0-9]*\|"prognosen":"[^"]*"' | tr '\n' ' '; echo
	echo "$dry" | grep -q '"state":"READY"' || { echo "Lauf nicht READY, kein Start" >&2; echo "$dry" | tail -3 >&2; exit 1; }

	"${SSH[@]}" "docker run -d --name $name --read-only --tmpfs /tmp --cpus 2 --memory 1g --user 65534:65534 \
		--env-file $DIR/.local/$id.docker.env $IMAGE >/dev/null"
	echo "gestartet um $(date +%H:%M); warte auf das Ende …"
fi

"${SSH[@]}" "docker wait $name >/dev/null; docker logs $name 2>&1" | python3 -c '
import collections, json, sys
acts, made, warn, last = collections.Counter(), set(), [], None
for line in sys.stdin:
    try:
        e = json.loads(line)
    except ValueError:
        continue
    if e.get("msg") == "aktion":
        acts[e["action"]] += 1
        if e["action"] == "create" and e.get("workload"):
            made.add(e["workload"])
    if e.get("level") in ("WARN", "ERROR"):
        warn.append(e)
    last = e
print("Aktionen:", dict(acts))
print(f"Workloads angelegt: {len(made)} von 15")
print("Warnungen/Fehler:", len(warn))
for e in warn[-5:]:
    print("  ", e.get("msg"), e.get("err", ""))
print("Ende:", last.get("msg") if last else "keine Logs")
'
