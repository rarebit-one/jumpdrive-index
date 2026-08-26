## What

<!-- What changes, and why. Link the issue: Closes #N -->

## Which layer?

- [ ] `domain` (pure vocabulary / resolve decision)
- [ ] `store` (a storage adapter — SQLite / Postgres — or the interface/conformance)
- [ ] `access` (identity: principals / spaces / lenses, or the Jumpdrive delegate)
- [ ] `mcp` / `httpapi` (the service surface)
- [ ] Integration (heyarr / jumpdrive-web)
- [ ] Tooling, docs, CI, deploy

## Checklist

- [ ] `make demo` passes (gofmt + vet + CGO-off build + `go test -race ./...`)
- [ ] Tests cover the new behaviour (not just the happy path); a storage change is exercised by `store/conformance`
- [ ] An ADR was added or updated if this changes an architectural stance
- [ ] Commits are signed off (`git commit -s`) — see CONTRIBUTING.md
