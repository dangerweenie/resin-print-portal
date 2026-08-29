# Resin Print Portal — build/test/deploy helpers.
# The central service is Go; `make help` lists everything.

IMAGE       ?= ghcr.io/dangerweenie/resin-portal
TAG         ?= dev
CHART       := deploy/helm/resin-portal
PIAGENT_OUT ?= bin/pi-agent-armv6
TEST_DB     ?= postgres://postgres:test@localhost:55432/portal?sslmode=disable

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the portal binary
	CGO_ENABLED=0 go build -trimpath -o bin/portal ./cmd/portal

.PHONY: pi-agent
pi-agent: ## Cross-compile the pi-agent static binary for the Pi Zero W (armv6)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 \
		go build -trimpath -ldflags='-s -w' -o $(PIAGENT_OUT) ./cmd/pi-agent
	@echo "built $(PIAGENT_OUT)"

.PHONY: test
test: ## Run unit tests (no database needed)
	go test ./...

.PHONY: test-integration
test-integration: ## Run all tests incl. the Postgres store tests (needs run-db or TEST_DATABASE_URL)
	# -p 1: the store/ and worker/ integration tests each migrate the shared
	# test database, so their packages must not run concurrently.
	TEST_DATABASE_URL=$(TEST_DB) go test -p 1 ./...

.PHONY: test-provision
test-provision: ## Off-hardware test of the SD-card provisioning + pi-agent install path
	bash provisioning/test-provision-sd.sh

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: run-db
run-db: ## Start a throwaway local Postgres on :55432 for tests / local runs
	docker run -d --rm --name portal-dev-pg \
		-e POSTGRES_PASSWORD=test -e POSTGRES_DB=portal \
		-p 55432:5432 postgres:16-alpine

.PHONY: stop-db
stop-db: ## Stop the throwaway local Postgres
	-docker stop portal-dev-pg

# Local run knobs — override on the command line, e.g.
#   make run ADMIN_PASSWORD=letmein
LOCAL_DSN      ?= $(TEST_DB)
ADMIN_PASSWORD ?= changeme
# A fixed dev session secret so admin logins survive a server restart.
DEV_SESSION_SECRET ?= dev-only-session-secret-not-for-production-32
STATUS_API_KEY ?= devstatuskey
# Optional fleet enrollment token. Empty (the default) = open enrollment, which
# is fine locally. Set one to require it (e.g. a public-facing portal).
ENROLL_TOKEN   ?=
TINKERACCESS_BASE_URL      ?= http://localhost:3000
TINKERACCESS_GET_USERS_PATH ?= /api/get_users11102523982452806591

.PHONY: run
run: build ## Migrate + run the API server in the foreground (needs run-db). Admin UI on :8080.
	DATABASE_URL="$(LOCAL_DSN)" bin/portal migrate up
	DATABASE_URL="$(LOCAL_DSN)" \
	SESSION_SECRET="$(DEV_SESSION_SECRET)" \
	ADMIN_USERNAME=captain ADMIN_PASSWORD="$(ADMIN_PASSWORD)" \
	STATUS_API_KEY="$(STATUS_API_KEY)" ENROLL_TOKEN="$(ENROLL_TOKEN)" \
	LISTEN_ADDR=:8080 LOG_LEVEL=debug \
	bin/portal server

.PHONY: run-worker
run-worker: build ## Run the roster-sync worker (needs a reachable TinkerAccess; override TINKERACCESS_BASE_URL)
	DATABASE_URL="$(LOCAL_DSN)" \
	TINKERACCESS_BASE_URL="$(TINKERACCESS_BASE_URL)" \
	TINKERACCESS_GET_USERS_PATH="$(TINKERACCESS_GET_USERS_PATH)" \
	SYNC_INTERVAL=1m LOG_LEVEL=debug \
	bin/portal worker

.PHONY: docker
docker: ## Build the container image
	docker build -f build/Dockerfile -t $(IMAGE):$(TAG) .

.PHONY: helm-deps
helm-deps: ## Fetch the chart's Bitnami postgresql dependency
	helm dependency build $(CHART)

.PHONY: helm-lint
helm-lint: helm-deps ## Lint + render the Helm chart
	helm lint $(CHART)
	helm template t $(CHART) --set app.adminPassword=x >/dev/null && echo "helm template OK"

.PHONY: clean
clean: ## Remove build output
	rm -rf bin $(CHART)/charts $(CHART)/*.tgz
