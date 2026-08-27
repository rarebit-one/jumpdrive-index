# Architecture decision records

Short records of decisions that were expensive to make and would be expensive to
reverse. Each states a decision, why, and — where it matters — what would make us
revisit it. An ADR that merely describes the code is not worth having.

| # | Decision | Status |
|---|---|---|
| [0001](0001-event-sourced-sor-dual-seams-agpl.md) | Event-sourced entity system-of-record, two swappable seams, AGPL | Accepted |
| [0002](0002-resolve-two-band-child-safety-as-principal.md) | Resolve-before-create is a two-band decision; child-safety is a principal, never a lens | Accepted |
| [0003](0003-external-id-uniqueness-scope.md) | External-ID uniqueness scope — per-space `UNIQUE(space, key)` | Accepted |
| [0004](0004-video-linking-maturity-ladder-and-out-of-process-media-toolchain.md) | Video linking is a maturity ladder; the media toolchain runs out-of-process | Accepted |
