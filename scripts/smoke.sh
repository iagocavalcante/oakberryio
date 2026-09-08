#!/usr/bin/env bash
# Runs on the Mac against a live oak box. Requires:
#   BOX        ssh target for the box, e.g. oak@10.0.0.5
#   OAK_DOMAIN the domain apps are served under (see docs/host.md)
#   oak        the CLI on PATH (`make build` puts a Mac build at bin/oak-darwin)
set -euo pipefail

: "${BOX:?set BOX=user@host}"
: "${OAK_DOMAIN:?set OAK_DOMAIN=apps.example.com}"

export OAK_API="https://oak.${OAK_DOMAIN}"

# docker push from the Mac must reach the box's registry at localhost:5000
# (see docs/host.md #6); forward it for the duration of this script.
ssh -N -L 5000:localhost:5000 "$BOX" &
FORWARD_PID=$!
cleanup() { kill "$FORWARD_PID" 2>/dev/null || true; }
trap cleanup EXIT
sleep 1

running_count() {
	oak status hello | awk '$2 == "running"' | wc -l | tr -d ' '
}

( cd examples/hello && oak deploy )
oak status hello | grep -q running
curl -fsS "https://hello.${OAK_DOMAIN}/health"
oak logs hello | grep -q "listening"

# A second deploy must swap the machine for a new one, not run both.
( cd examples/hello && oak deploy )
count=$(running_count)
if [ "$count" -ne 1 ]; then
	echo "expected exactly one running machine after a second deploy, got $count" >&2
	exit 1
fi
curl -fsS "https://hello.${OAK_DOMAIN}/health"

# oakd restart must bring the existing machine back via Reconcile, with no
# redeploy needed.
ssh "$BOX" 'sudo systemctl restart oakd'
sleep 15
curl -fsS "https://hello.${OAK_DOMAIN}/health"

echo SMOKE OK
