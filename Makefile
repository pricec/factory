GOBIN ?= $(HOME)/go/bin
export PATH := $(GOBIN):$(PATH)

GOFUMPT := $(GOBIN)/gofumpt
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
	go build $(CMDS)

release:
	GOOS=linux GOARCH=amd64 go build -o dist/factory-linux-amd64 ./cmd/factory
	GOOS=linux GOARCH=arm64 go build -o dist/factory-linux-arm64 ./cmd/factory
	GOOS=linux GOARCH=amd64 go build -o dist/foreman-linux-amd64 ./cmd/foreman
	GOOS=linux GOARCH=arm64 go build -o dist/foreman-linux-arm64 ./cmd/foreman

DOCKER_GOIMG ?= golang:1.27

release-docker:
	docker run --rm \
		-v "$(CURDIR):/src" -w /src \
		-v factory-gomodcache:/go/pkg/mod \
		-v factory-gocache:/root/.cache/go-build \
		-e CGO_ENABLED=0 \
		"$(DOCKER_GOIMG)" sh -ec '\
			GOOS=linux GOARCH=amd64 go build -o dist/factory-linux-amd64 ./cmd/factory && \
			GOOS=linux GOARCH=arm64 go build -o dist/factory-linux-arm64 ./cmd/factory && \
			GOOS=linux GOARCH=amd64 go build -o dist/foreman-linux-amd64 ./cmd/foreman && \
			GOOS=linux GOARCH=arm64 go build -o dist/foreman-linux-arm64 ./cmd/foreman'
	-docker run --rm -v "$(CURDIR):/src" -w /src "$(DOCKER_GOIMG)" \
		chown -R $$(id -u):$$(id -g) dist

ci: fmt-check vet test build
