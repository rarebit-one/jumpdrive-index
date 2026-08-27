# ADR-0004: Video linking is a maturity ladder; the media toolchain runs out-of-process

- **Status:** **Accepted** (records the M4/M6 stance; rung 3 deferred, written 2026-08-27)
- **Date:** 2026-08-27
- **Deciders:** jumpdrive-index maintainers
- **Milestone:** M4 (rung 1, done), M6 rung 2 (done), M6 rung 3 (deferred aspiration)

## Context

YouTube analysis videos ("@kroft_movies on Alien") link into the graph: a video is *about* a
heyarr work. **How precise that link is** — the whole video, a timecoded span, or a specific
frame — is a ladder, and building the top rung first would be a large, ML-heavy effort for
uncertain payoff. Separately, extracting a video's metadata and transcript needs `yt-dlp` and
Whisper, which pull in heavy, non-Go, occasionally-native dependencies — and the entire storage
design (pure-Go, `CGO_ENABLED=0`, distroless/scratch) exists precisely to avoid that. Both
questions needed a stance before the ingestion code committed to one.

## Decision

### 1. YouTube ingestion lives in the index, not in heyarr

heyarr's content types are closed (movie/series/music/book). A YouTube video is a **Starchart
`VideoObject`/`Organization` node with an `about` edge to a heyarr `work_id`** — referenced by
id, never a copy of heyarr's catalogue (see ADR-0003's external-id schemes and heyarr ADR-0050
for the reverse lookup that resolves `tmdb → work_id`).

### 2. The link precision is a three-rung ladder, built bottom-up

- **Rung 1 — item level (M4, shipped).** A `VideoObject` + a channel `Organization` + a
  `VideoObject --about--> <heyarr-work ref>` edge. "This video is about Alien."
- **Rung 2 — timecode (M6, shipped).** A transcript segment becomes a schema.org **`Clip`**
  (`startOffset`/`endOffset` in seconds on the transcript timeline), `Clip --isPartOf-->
  VideoObject`, carrying its **own** `about` edge to the work. "At 3:12–4:05 this is about
  Alien." Rung 1's video-level link is preserved; rung 2 is additive; a Clip is idempotent via a
  `youtube-clip` external id.
- **Rung 3 — frame level (deferred aspiration, OUT OF SCOPE).** Keyframe extraction + visual
  embeddings to link a *frame*, not a transcript span. Recorded here as the ladder's top so the
  intent is not lost, but explicitly **not built**: it requires a visual-embedding model and
  keyframe pipeline that would drag ML/CGO weight into the core (see §3), and its payoff over
  rung 2 is unproven. Revisit only on a measured need, and only if it can stay out-of-process.

*Why a ladder:* each rung is independently useful and testable; rung 2 reuses rung 1's
reconciliation (`resolveHeyarrWork`) rather than replacing it; and the expensive, speculative
rung 3 is never on the critical path.

### 3. The external media toolchain is optional, capability-routed, and out-of-process

`yt-dlp` (metadata) and Whisper (transcript) are an **optional external capability** behind an
interface, with a real shell-out implementation and a null/mock implementation, mirroring
heyarr's ADR-0025 shape. When the capability is absent, ingestion **degrades to metadata-only /
no-transcript** — never a hard failure. The main binary stays pure-Go / `CGO_ENABLED=0` /
distroless: the toolchain runs **out-of-process** (shelled subprocesses / a capability-routed
job), so no ML or native dependency is ever linked into the index itself.

*Why out-of-process:* it preserves the static-binary/distroless guarantee the whole storage
design was built to keep (ADR-0001), and it makes the toolchain a deployment concern (present or
not on a given host) rather than a compile-time one.

## Consequences

- Rung 1 and rung 2 are shipped and CI-tested against fixtures (a canned metadata + transcript
  through the null/mock capability); the demo loop proves the nodes/edges without any real
  binary present.
- Deploying real ingestion means installing `yt-dlp`/Whisper on the host and pointing the
  capability at them; the index binary is unchanged. Systemd resource caps and whether a
  download dir needs a `ReadWritePaths` drop-in are a **deploy-time** decision, pending
  measurement of the real Whisper footprint (tracked, not fixed here).
- Rung 3 stays a recorded aspiration; nothing depends on it, and it cannot regress the CGO-off
  guarantee if/when it is built.
- **Revisit if** a measured need for frame-level linking appears AND a visual-embedding path
  exists that keeps the core CGO-free (e.g. an out-of-process embedding capability).
