#!/usr/bin/env bash
# wait-healthy.sh — block until every codeintel-stack service
# reports "healthy" via Docker's per-container healthcheck, or
# exit non-zero on timeout.
#
# Called by the codeintel Makefile's `stack-up` target. Kept as
# a stand-alone script so the bash control flow (multi-line
# while loop, conditional, jq parsing) survives Make's recipe
# transcription cleanly — `.ONESHELL` interactions with multi-
# line bash scripts vary across Make versions, so we sidestep
# the question entirely.

set -euo pipefail

COMPOSE="${COMPOSE:-docker compose -f docker-compose.yml}"
TIMEOUT="${HEALTHCHECK_TIMEOUT:-60}"
INTERVAL="${HEALTHCHECK_INTERVAL:-2}"

# Services whose docker-level healthcheck we wait on. nebula-storaged
# is intentionally NOT in this list — Nebula 3.x storaged requires
# the ADD HOSTS console-init step before its healthcheck passes,
# and that init runs AFTER graphd is healthy. By the time graphd
# is healthy, storaged will be registered + ready.
SERVICES=(
    codeintel-postgres
    codeintel-redis
    nebula-metad
    nebula-graphd
)

deadline=$(( $(date +%s) + TIMEOUT ))

while :; do
    all_healthy=true
    line=""
    for svc in "${SERVICES[@]}"; do
        status=$($COMPOSE ps --format json "$svc" 2>/dev/null \
            | jq -r '.Health // "missing"' 2>/dev/null \
            || echo "missing")
        line+=$(printf "%s=%-10s " "$svc" "$status")
        if [ "$status" != "healthy" ]; then
            all_healthy=false
        fi
    done
    printf "  %s\n" "$line"

    if $all_healthy; then
        echo "## wait-healthy: all services healthy"
        exit 0
    fi
    if [ "$(date +%s)" -gt "$deadline" ]; then
        echo "## wait-healthy: TIMEOUT after ${TIMEOUT}s"
        exit 1
    fi
    sleep "$INTERVAL"
done
