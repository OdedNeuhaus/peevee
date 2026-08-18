IMAGE   ?= peevee
TAG     ?= 0.1.0
VERSION ?= $(TAG)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
NAMESPACE ?= peevee

LDFLAGS = -s -w \
  -X github.com/OdedNeuhaus/peevee/internal/version.Version=$(VERSION) \
  -X github.com/OdedNeuhaus/peevee/internal/version.Commit=$(COMMIT) \
  -X github.com/OdedNeuhaus/peevee/internal/version.Date=$(DATE)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into ./peevee
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o peevee ./cmd/peevee

.PHONY: run
run: ## Run locally against your current kubeconfig
	go run ./cmd/peevee --config ./config.local.yaml --log-level=debug

.PHONY: test
test: ## Run all tests
	go test ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: vet ## Vet plus chart lint
	helm lint charts/peevee

.PHONY: image
image: ## Build the container image
	docker build -t $(IMAGE):$(TAG) \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

.PHONY: check
check: vet test lint ## Everything CI runs

.PHONY: clean
clean:
	rm -f peevee coverage.out
