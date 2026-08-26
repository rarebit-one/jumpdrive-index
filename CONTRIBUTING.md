# Contributing

## Workflow

- **Worktree-only.** Do work on a branch in a git worktree, not the main
  checkout; open a PR into `main`. `main` is protected against deletion and
  force-push.
- **Signed commits with a DCO sign-off.** Every commit is both cryptographically
  signed and carries a `Signed-off-by` line — use `git commit -s` (signing is
  configured globally). We do not use a CLA; the DCO sign-off is the contribution
  agreement.
- **Conventional Commits** for the subject line (`feat:`, `fix:`, `chore:`,
  `refactor:`, `docs:`, `test:`).
- **Squash merge.** PRs land as a single squashed commit; the branch is deleted
  on merge.

## Before you open a PR

```bash
make demo   # gofmt + go vet + CGO-off build + go test -race ./...
```

`make demo` is the local gate and mirrors CI. A change to a storage adapter must
be exercised by the cross-adapter suite in `internal/store/conformance` — that is
what keeps the SQLite and Postgres adapters from diverging.

## Architecture stances go in an ADR

If a change alters an architectural stance (a seam, the resolve policy, the
access model, an integration contract), add or update an ADR under `docs/adr/`
following the estate format (Status/Date, Context, Decision, Consequences,
Alternatives rejected). Record what would make us revisit the decision.

## License

By contributing you agree your work is licensed under **AGPL-3.0-or-later**, the
license of this repository.
