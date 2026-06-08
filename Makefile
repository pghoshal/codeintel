# codeintel local dev lifecycle. Phase A.1 of the re-prioritized
# port plan; see docs/codeintel-port-gap-inventory.md §7.
#
# Recipe bodies are delegated to ./tools/*.sh so the bash control
# flow (loops, conditionals) doesn't have to survive Make's
# recipe-line semantics (which interact badly with .ONESHELL
# across Make versions).
#
# The healthcheck target is the binding Phase A.1 E2E gate:
# real-client probes against Postgres / Redis / Nebula; non-zero
# exit on any failure.

SHELL := bash

COMPOSE := docker compose -f docker-compose.yml

# Per-service connection params. Tools/healthcheck.sh and
# tools/wait-healthy.sh inherit these via the environment.
export COMPOSE
export PG_HOST            := 127.0.0.1
export PG_PORT            := 5433
export PG_USER            := codeintel
export PG_DB              := codeintel
export REDIS_HOST         := 127.0.0.1
export REDIS_PORT         := 6380
export NEBULA_NETWORK     := codeintel_default
export NEBULA_GRAPHD_HOST := codeintel-nebula-graphd
export NEBULA_GRAPHD_PORT := 9669
export NEBULA_USER        := root
export NEBULA_PASSWORD    := nebula

# Per-service healthcheck timeouts (seconds). Operators tune via
# `make stack-up HEALTHCHECK_TIMEOUT=120`.
export HEALTHCHECK_TIMEOUT  ?= 60
export HEALTHCHECK_INTERVAL ?= 2

.PHONY: help stack-up stack-down stack-down-volumes stack-healthcheck stack-status product-gate

help:
	@echo "## codeintel make targets"
	@echo ""
	@echo "  stack-up           Bring every service up; block on docker health + real-client gate"
	@echo "  stack-down         Graceful shutdown; data volumes preserved"
	@echo "  stack-down-volumes Full wipe — data volumes deleted (DESTRUCTIVE; prompts for confirmation)"
	@echo "  stack-healthcheck  Real-client probe of every service; non-zero exit on any failure"
	@echo "  stack-status       Print docker compose ps for the stack"
	@echo "  product-gate       Non-skipping Kind/Docker ten-repo product-quality gate"
	@echo ""
	@echo "Ports:"
	@echo "  Postgres : $(PG_HOST):$(PG_PORT) (user=$(PG_USER) db=$(PG_DB))"
	@echo "  Redis    : $(REDIS_HOST):$(REDIS_PORT)"
	@echo "  Nebula   : 127.0.0.1:9669 (graphd) / 9559 (metad) / 9779 (storaged)"

stack-up:
	@echo "## stack-up: bringing up postgres + redis + nebula"
	$(COMPOSE) up -d
	@echo "## stack-up: waiting for docker-level healthy state"
	bash tools/wait-healthy.sh
	@echo "## stack-up: running real-client healthcheck gate"
	bash tools/healthcheck.sh

stack-down:
	@echo "## stack-down: graceful shutdown; volumes preserved"
	$(COMPOSE) down

stack-down-volumes:
	@echo "## stack-down-volumes: WIPING all data volumes"
	@read -p "  Type 'yes' to confirm: " confirm; \
		if [ "$$confirm" != "yes" ]; then echo "  aborted"; exit 1; fi
	$(COMPOSE) down -v

stack-status:
	$(COMPOSE) ps

stack-healthcheck:
	bash tools/healthcheck.sh

product-gate:
	bash tools/product-gate.sh
