# jumpdrive-index — developer entrypoints. `make demo` is the milestone gate
# (mirrors heyarr-core): it fails if a claimed mechanism was never exercised.

.PHONY: build test fmt vet lint demo tidy

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

# demo == the acceptance gate. Grows into scripts/acceptance.sh as milestones land.
demo: lint build test
	@echo "OK: fmt+vet+build+test green"
