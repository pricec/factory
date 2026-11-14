GOBIN ?= $(HOME)/go/bin
export PATH := $(GOBIN):$(PATH)

GOFUMPT := $(GOBIN)/gofumpt
CMDS := ./cmd/factory ./cmd/foreman ./cmd/worker

.PHONY: gofumpt fmt fmt-check vet test build ci

gofumpt:
	@test -x "$(GOFUMPT)" || GOBIN="$(GOBIN)" go install mvdan.cc/gofumpt@latest

fmt: gofumpt
	$(GOFUMPT) -w ./cmd ./internal

fmt-check: gofumpt
	@out=`"$(GOFUMPT)" -l ./cmd ./internal`; \
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

ci: fmt-check vet test build
