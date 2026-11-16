GOBIN ?= $(HOME)/go/bin
export PATH := $(GOBIN):$(PATH)

GOFUMPT := $(GOBIN)/gofumpt
# Build id (spec §8.4, auto-update C1): <UTC commit time>-<12-char sha>.
# Computed on the host (the release-docker container has no .git).
BUILD_ID ?= $(shell TZ=UTC git log -1 --format=%cd --date=format-local:%Y%m%dT%H%M%SZ 2>/dev/null)-$(shell git rev-parse --short=12 HEAD 2>/dev/null)
LDFLAGS := -X github.com/pricec/factory/internal/buildinfo.ID=$(BUILD_ID)
CMDS := ./cmd/factory ./cmd/foreman ./cmd/worker

.PHONY: gofumpt fmt fmt-check vet test build release release-docker ci

gofumpt:
	@test -x "$(GOFUMPT)" || GOBIN="$(GOBIN)" go install mvdan.cc/gofumpt@latest

fmt: gofumpt
	$(GOFUMPT) -w ./cmd ./internal ./test

fmt-check: gofumpt
	@out=`"$(GOFUMPT)" -l ./cmd ./internal ./test`; \
	if [ -n "$$out" ]; then \
		echo "files are not gofumpt-clean:"; \
		echo "$$out"; \
		exit 1; \
	fi
	echo "gofumpt: clean"

vet:
	go vet ./...

test:
	go test ./...

build:
	go build -ldflags "$(LDFLAGS)" $(CMDS)

release:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/factory-linux-amd64 ./cmd/factory
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/factory-linux-arm64 ./cmd/factory
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/foreman-linux-amd64 ./cmd/foreman
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/foreman-linux-arm64 ./cmd/foreman
	go run ./internal/tools/mkmanifest -dir dist -build "$(BUILD_ID)"

DOCKER_GOIMG ?= golang:1.27

release-docker:
	docker run --rm \
		-v "$(CURDIR):/src" -w /src \
		-v factory-gomodcache:/go/pkg/mod \
		-v factory-gocache:/root/.cache/go-build \
		-e CGO_ENABLED=0 -e BUILD_ID="$(BUILD_ID)" \
		"$(DOCKER_GOIMG)" sh -ec '\
			GOOS=linux GOARCH=amd64 go build -buildvcs=false -ldflags "-X github.com/pricec/factory/internal/buildinfo.ID=$$BUILD_ID" -o dist/factory-linux-amd64 ./cmd/factory && \
			GOOS=linux GOARCH=arm64 go build -buildvcs=false -ldflags "-X github.com/pricec/factory/internal/buildinfo.ID=$$BUILD_ID" -o dist/factory-linux-arm64 ./cmd/factory && \
			GOOS=linux GOARCH=amd64 go build -buildvcs=false -ldflags "-X github.com/pricec/factory/internal/buildinfo.ID=$$BUILD_ID" -o dist/foreman-linux-amd64 ./cmd/foreman && \
			GOOS=linux GOARCH=arm64 go build -buildvcs=false -ldflags "-X github.com/pricec/factory/internal/buildinfo.ID=$$BUILD_ID" -o dist/foreman-linux-arm64 ./cmd/foreman && \
			go run -buildvcs=false ./internal/tools/mkmanifest -dir dist -build "$$BUILD_ID"'
	-docker run --rm -v "$(CURDIR):/src" -w /src "$(DOCKER_GOIMG)" \
		chown -R $$(id -u):$$(id -g) dist

ci: fmt-check vet test build
