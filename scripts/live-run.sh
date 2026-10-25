#!/usr/bin/env bash
# Starts the orchestrator for a new run on hetzner and waits for it to end.
#
#	scripts/live-run.sh                         # newest greenops-<id>.env from ~/Downloads
#	scripts/live-run.sh ~/Downloads/greenops-85e472c9e6.env
#	IMAGE=greenops-orch-go:v3 scripts/live-run.sh
#	ARGS=-global scripts/live-run.sh --new      # extra flags for the orchestrator
#	scripts/live-run.sh --new [name] [speed]    # creates the run in the portal itself (scripts/portal, log in once)
#	scripts/live-run.sh --watch                 # starts every newly downloaded run automatically
#
# Credentials are never printed: the file goes to the server unread via scp, only key names are
# shown. Runs that already have a container are not restarted.
set -euo pipefail

HOST=${HOST:-hetzner-wikidata}
DIR=${DIR:-/data/infra-kg-austria/work/greenops-orch-go}
IMAGE=${IMAGE:-greenops-orch-go:v6}
ARGS=${ARGS:-}
SSH=(ssh -o ClearAllForwardings=yes "$HOST")

if [[ ${1:-} == --new ]]; then
	portal=$(cd "$(dirname "$0")/portal" && node portal.mjs new-run "${2:-auto $(date +%H:%M)}" "${3:-720}")
	echo "$portal"
	set -- "$(sed -n 's/^Saved file: //p' <<<"$portal")"
fi

if [[ ${1:-} == --watch ]]; then
	# files already present at startup count as done; after that every new one exactly once.
	seen=$(ls ~/Downloads/greenops-*.env 2>/dev/null || true)
	echo "waiting for new greenops-<id>.env in ~/Downloads (Ctrl-C quits) …"
	while sleep 5; do
		for f in ~/Downloads/greenops-*.env; do
			[[ -f $f ]] && ! grep -qxF "$f" <<<"$seen" || continue
			seen+=$'\n'$f
			sleep 2 # let the download finish writing
			"$0" "$f" || echo "run $f failed, still waiting" >&2
		done
	done
fi

env_file=${1:-$(ls -t ~/Downloads/greenops-*.env 2>/dev/null | head -1)}
[[ -f $env_file ]] || { echo "no greenops-<id>.env found" >&2; exit 1; }
id=$(basename "$env_file" .env)
id=${id#greenops-}
name=greenops-$id
echo "run $id, image $IMAGE $ARGS"

if "${SSH[@]}" "docker ps -a --format '{{.Names}}' | grep -qx $name"; then
	echo "container $name already exists; result:" >&2
else
	scp -q -o ClearAllForwardings=yes "$env_file" "$HOST:$DIR/.local/$id.env"
	"${SSH[@]}" "cd $DIR/.local && chmod 600 $id.env &&
		sed -e 's/^export //' -e 's/=\"\(.*\)\"\$/=\1/' -e \"s/='\(.*\)'\$/=\1/\" $id.env > $id.docker.env &&
		chmod 600 $id.docker.env && grep -v '^#' $id.docker.env | cut -d= -f1 | tr '\n' ' '; echo"

	echo "dry run …"
	dry=$("${SSH[@]}" "timeout 60 docker run --rm --read-only --tmpfs /tmp --user 65534:65534 \
		--env-file $DIR/.local/$id.docker.env $IMAGE -dry $ARGS 2>&1")
	echo "$dry" | grep -o '"state":"[A-Z_]*"\|"speed":[0-9]*\|"workloads":[0-9]*\|"forecasts":"[^"]*"' | tr '\n' ' '; echo
	echo "$dry" | grep -q '"state":"READY"' || { echo "run not READY, not starting" >&2; echo "$dry" | tail -3 >&2; exit 1; }

	"${SSH[@]}" "docker run -d --name $name --read-only --tmpfs /tmp --cpus 2 --memory 1g --user 65534:65534 \
		--env-file $DIR/.local/$id.docker.env $IMAGE $ARGS >/dev/null"
	echo "started at $(date +%H:%M); waiting for the end …"
fi

"${SSH[@]}" "docker wait $name >/dev/null; docker logs $name 2>&1" | python3 -c '
import collections, json, sys
acts, made, warn, last = collections.Counter(), set(), [], None
for line in sys.stdin:
    try:
        e = json.loads(line)
    except ValueError:
        continue
    if e.get("msg") == "action":
        acts[e["action"]] += 1
        if e["action"] == "create" and e.get("workload"):
            made.add(e["workload"])
    if e.get("level") in ("WARN", "ERROR"):
        warn.append(e)
    last = e
print("actions:", dict(acts))
print(f"workloads created: {len(made)} of 15")
print("warnings/errors:", len(warn))
for e in warn[-5:]:
    print("  ", e.get("msg"), e.get("err", ""))
print("end:", last.get("msg") if last else "no logs")
'
