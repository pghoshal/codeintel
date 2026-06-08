#!/usr/bin/env bash
# healthcheck.sh — real-client probes against every codeintel-stack
# service. Returns 0 only when ALL services respond to a real
# client request:
#
#   - Postgres : pg_isready -h $PG_HOST -p $PG_PORT
#   - Redis    : redis-cli ping  (expects "PONG")
#   - Nebula   : nebula-console -e 'SHOW HOSTS'
#
# Each probe uses the canonical client for the service (matched
# to the server version where possible) — the same image
# operators reach for at the CLI. A passing run proves the
# protocol round-trip works, not just that the container is
# "running".

set -uo pipefail

PG_HOST="${PG_HOST:-127.0.0.1}"
PG_PORT="${PG_PORT:-5433}"
PG_USER="${PG_USER:-codeintel}"
PG_DB="${PG_DB:-codeintel}"

REDIS_HOST="${REDIS_HOST:-127.0.0.1}"
REDIS_PORT="${REDIS_PORT:-6380}"

NEBULA_NETWORK="${NEBULA_NETWORK:-codeintel_default}"
NEBULA_GRAPHD_HOST="${NEBULA_GRAPHD_HOST:-codeintel-nebula-graphd}"
NEBULA_GRAPHD_PORT="${NEBULA_GRAPHD_PORT:-9669}"
NEBULA_USER="${NEBULA_USER:-root}"
NEBULA_PASSWORD="${NEBULA_PASSWORD:-nebula}"

echo "## healthcheck: probing services with real clients"

pg_ok=0
redis_ok=0
nebula_ok=0

# Postgres probe. pg_isready is the canonical "can clients connect
# and authenticate" check. -t 5 caps the network wait so a hung
# server doesn't make the gate hang forever.
if docker run --rm --network host \
    -e PGPASSWORD=codeintel \
    postgres:16-alpine \
    pg_isready -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" -d "$PG_DB" -t 5 \
    >/tmp/codeintel-pg-probe.log 2>&1; then
    echo "  ✓ postgres @ $PG_HOST:$PG_PORT"
    pg_ok=1
else
    echo "  ✗ postgres @ $PG_HOST:$PG_PORT — see /tmp/codeintel-pg-probe.log"
    sed 's/^/    /' /tmp/codeintel-pg-probe.log
fi

# Redis probe. PING is the cheapest valid command; PONG is the
# only correct reply. Uses redis-cli from the same major version
# the server runs.
redis_response=$(docker run --rm --network host redis:7-alpine \
    redis-cli -h "$REDIS_HOST" -p "$REDIS_PORT" ping 2>&1 || true)
if [ "$redis_response" = "PONG" ]; then
    echo "  ✓ redis @ $REDIS_HOST:$REDIS_PORT"
    redis_ok=1
else
    echo "  ✗ redis @ $REDIS_HOST:$REDIS_PORT (got: $redis_response)"
fi

# Nebula probe. nebula-console connects to graphd, authenticates,
# runs SHOW HOSTS (the canonical cluster-status query), and
# disconnects. Exit 0 from the console proves the protocol round-
# trip succeeded; we don't grep the output because the format
# varies across minor versions.
#
# Runs inside the compose-managed bridge network so the in-network
# hostname graphd advertises (codeintel-nebula-graphd) resolves.
# Production clients use the same network path.
if docker run --rm --network "$NEBULA_NETWORK" vesoft/nebula-console:v3.8.0 \
    -addr "$NEBULA_GRAPHD_HOST" -port "$NEBULA_GRAPHD_PORT" \
    -u "$NEBULA_USER" -p "$NEBULA_PASSWORD" \
    -e 'SHOW HOSTS' >/tmp/codeintel-nebula-probe.log 2>&1; then
    echo "  ✓ nebula @ graphd:$NEBULA_GRAPHD_PORT"
    nebula_ok=1
else
    echo "  ✗ nebula @ graphd:$NEBULA_GRAPHD_PORT — see /tmp/codeintel-nebula-probe.log"
    tail -5 /tmp/codeintel-nebula-probe.log | sed 's/^/    /'
fi

if [ "$pg_ok" -eq 1 ] && [ "$redis_ok" -eq 1 ] && [ "$nebula_ok" -eq 1 ]; then
    echo "## healthcheck: PASS"
    exit 0
fi

echo "## healthcheck: FAIL"
exit 1
