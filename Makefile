# jumpdrive-index — developer entrypoints. `make demo` is the milestone gate
# (mirrors heyarr-core): it fails if a claimed mechanism was never exercised.

.PHONY: build test fmt vet lint demo acceptance tidy

build:
	CGO_ENABLED=0 go build ./...

test:
	go test -race ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

# lint gate: gofmt must produce no output, then vet.
lint:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needs to run on:"; echo "$$out"; exit 1; fi
	go vet ./...

tidy:
	go mod tidy

# acceptance: build+serve the binary and drive an end-to-end MCP-over-HTTP loop.
acceptance:
	@./scripts/acceptance.sh

# demo == the milestone gate: static checks + the whole test suite + the live
# serve loop. It fails if a claimed mechanism (the running service) was never
# exercised.
demo: lint build test acceptance
	@echo "OK: fmt+vet+build+test+acceptance green"
