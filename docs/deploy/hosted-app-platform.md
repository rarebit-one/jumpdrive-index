# Hosted deploy — DigitalOcean App Platform (Track B)

How to run the **hosted** build of `jumpdrive-index`: Postgres-backed, built from
source by DigitalOcean App Platform. This is a *different* artifact from the
Starchart homelab deploy — see [`reference-linux-host.md`](reference-linux-host.md)
for that one. Mirrors the `jumpdrive-broker` template.

## Two build paths, one binary

| | Starchart (homelab) | Hosted (App Platform) |
|---|---|---|
| Artifact | `deploy/docker/Dockerfile` — distroless image that takes a **prebuilt** binary from the release pipeline | root `Dockerfile` — **builds the binary in-image** (`golang` builder → `scratch`), because App Platform builds from source |
| Backend | `sqlite` (owns all its own bytes) | `postgres` (a dedicated managed DB) |
| Identity | `starchart` (self-contained principals) | `jumpdrive` (delegate to an authorizer) |
| Runs under | systemd on a Linux host | App Platform service + PRE_DEPLOY job |
| Spec | `deploy/systemd/*` | `.do/app.yaml` |

Both are `CGO_ENABLED=0` static builds of `./cmd/jumpdrive-index`; the store
backends (`modernc.org/sqlite`, `jackc/pgx`) are pure Go, so the runtime is
`scratch`/distroless with no libc.

## Migrations: a PRE_DEPLOY phase (`MODE=migrate`)

Migrations do **not** run at container start. The `.do/app.yaml` declares a
`PRE_DEPLOY` job that runs the *same image* with `JDX_MODE=migrate`; the service
runs it with `JDX_MODE=serve`. Rationale (same as jumpdrive-broker):

- A migration failure at boot kills the process before it binds a port, so the
  only symptom is a health-check timeout that says nothing about migrations.
- As its own phase it fails with a readable error and the **previous version keeps
  serving**.
- It decouples migration from `instance_count` — boot-time migration with N
  replicas is N racing writers.

`JDX_MODE` is an **env**, not a `run_command`: the runtime image is `scratch`, so
there is no shell for App Platform to invoke.

## Dual health probes

App Platform wires **two** probes at endpoints already served by
`internal/httpapi`:

- `health_check` → `GET /health/ready` — the **rotation** gate. A failing instance
  is pulled from rotation but not killed.
- `liveness_health_check` → `GET /health/alive` — the **restart** gate. A wedged
  process is restarted.

They are not redundant: an app with a single `health_check` cannot express "this
process is wedged, kill it" separately from "this process cannot serve right now".

## Required secrets / env

Set out of band against the **live** app (never committed — the spec carries the
shape, not the values):

| Env | Type | Notes |
|-----|------|-------|
| `JDX_JUMPDRIVE_SECRET` | **SECRET** | shared secret for the Jumpdrive authorizer (identity=jumpdrive) |
| `JDX_HEYARR_TOKEN` | **SECRET**, optional | only with `JDX_HEYARR_URL`; a token with no URL is a hard boot refusal |

Plain (in-spec) config: `JDX_MODE`, `JDX_BACKEND=postgres`, `JDX_HTTP_ADDR=":8090"`,
`JDX_AUTH=true`, `JDX_IDENTITY=jumpdrive`, `JDX_JUMPDRIVE_URL` (placeholder — point
at the real authorizer). `JDX_DSN` is bound to the managed database via
`${jumpdrive-index-db.DATABASE_URL}` and substituted at deploy time.

> **Auth posture (ADR-0011):** the service binds a routable address on App
> Platform, so `JDX_AUTH=true` is mandatory — the process refuses to boot
> unauthenticated on a non-loopback bind.

## Dedicated dev database

`.do/app.yaml` declares its **own** managed Postgres (`jumpdrive-index-db`, PG 16,
`production: false`, ~$7/mo) rather than a shared cluster — sharing would couple
this service's lifecycle to another app's (a resize or connection-limit exhaustion
elsewhere would hit it). It is a dev database, not yet a durable system of record.

## Applying spec changes

🔴 Do **not** `doctl apps update --spec .do/app.yaml` directly — a committed spec
has empty `SECRET` values, so a blind apply wipes the live secrets. Fetch the live
spec, edit it, apply it back:

```bash
doctl --context rarebit apps spec get <app-id> > /tmp/live.yaml
# edit /tmp/live.yaml
doctl --context rarebit apps update <app-id> --spec /tmp/live.yaml
```
