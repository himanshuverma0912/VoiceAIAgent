# The whole platform on your laptop (see dev/README.md):
#   make dev       start it (the first time: also makes dev/.env with local secrets)
#   make down      stop it; your local flows, users and changes stay
#   make reset     stop it and delete all local data; the next `make dev` starts fresh
#   make logs      follow the logs (one service: make logs s=agent)
#   make ps        what's running
#   make config    rebuild the config service after a Go change
#   make rebuild   rebuild after changing requirements.txt, package.json or a Dockerfile

COMPOSE = docker compose -f dev/compose.yaml --env-file dev/.env
s ?=

.PHONY: dev down reset logs ps config rebuild

dev:
	@python3 dev/init.py
	$(COMPOSE) up -d --build --remove-orphans
	@echo "Setting up demo users, flows and integrations (Keycloak takes a minute the first time)..."
	@$(COMPOSE) wait seed >/dev/null; \
	code=$$($(COMPOSE) ps -a seed --format '{{.ExitCode}}'); \
	$(COMPOSE) logs --no-log-prefix seed; \
	[ "$$code" = "0" ] || { echo "The setup failed (above). Fix it and run make dev again."; exit 1; }

down:
	$(COMPOSE) down

reset:
	$(COMPOSE) down --volumes

logs:
	$(COMPOSE) logs --follow --tail 100 $(s)

ps:
	$(COMPOSE) ps

config:
	$(COMPOSE) up -d --build config-service

rebuild:
	$(COMPOSE) build --pull
	$(COMPOSE) up -d --force-recreate
