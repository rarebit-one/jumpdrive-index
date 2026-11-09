# Hosted deploy: DigitalOcean App Platform

How the **hosted** build of `jumpdrive-index` runs: backed by Postgres, using the
`jumpdrive` identity, and serving one Jumpdrive control plane. It's a different
deploy from the Starchart homelab one; [`reference-linux-host.md`](reference-linux-host.md)
covers that.

## Where it runs

| | Staging | Production |
|---|---|---|
| Host | hyperion-2, homelab-ops role `jumpdrive-index-staging` | an **internal service in the `jumpdrive-web` DO app**, next to `control-plane` (owner decision 2026-11-03) |
| Image | `ghcr.io/rarebit-one/jumpdrive-index@sha256:…`, pinned by digest | the same image, pinned by digest in the live app spec |
| Store | a dedicated pgvector Postgres 16 container | database `jumpdrive_index` on the `jumpdrive-pg` managed cluster (PG 16 + `vector`) |
| Migrations | `jd-index-migrate.service` (`JDX_MODE=migrate`) | the `jumpdrive-index-migrate` PRE_DEPLOY job (`JDX_MODE=migrate`) |
| Reached at | `http://jd-index:8090` on the podman bridge | `http://jumpdrive-index:8090`, internal to the app, with no public route |

**The production spec and rollout live in jumpdrive-web**, because the index is one of
that app's components:

- [`docs/index-production-rollout.md`](https://github.com/rarebit-one/jumpdrive-web/blob/main/docs/index-production-rollout.md)
  is the plan, the env, the rollout steps and their rollbacks;
- its `index-spec.jq` is the live-spec edit;
- `.do/app.yaml` there is the committed record.

This repo no longer carries an App Platform spec. The standalone app it used to
describe, with a source build and an App Platform dev database, was never deployed,
and the owner's decision replaced it.

## The image: built on the runner, not on App Platform

`.github/workflows/publish-image.yml` builds the static `linux/amd64` binary on the
runner with the private-module auth that `ci.yml` uses, then packages it with
`deploy/ghcr/Dockerfile` (`scratch`, CA bundle, uid 65532, port 8090, no shell). It
pushes `sha-<full sha>`, which is immutable, and `main`, which is a convenience tag.
Deploys pin the **digest** from the job summary.

App Platform can't build this repo from source: an in-image build would need to
fetch the private `void-which-binds-go` module unauthenticated, which fails. App
Platform also never auto-deploys GHCR images, so every bump is a deliberate,
reviewed change to the pinned digest. That matches the staging policy.

The package is **public**; the owner accepted this on 2026-10-26. The compiled
void-which-binds-go is exposed, but not its source. App Platform pulls the image
without `registry_credentials`.

## Migrations: their own phase (`JDX_MODE=migrate`)

Migrations never run at serve start. The same image runs with `JDX_MODE=migrate`, as
a PRE_DEPLOY job on App Platform and as a oneshot unit on staging. `JDX_MODE` is an
env var and not a `run_command` because the image is `scratch` and has no shell.
Running them as a separate phase has three benefits:

- A failed migration fails with a readable error, and the previous version keeps
  serving. A boot-time failure would show up only as a health-check timeout.
- Migrations are decoupled from `instance_count`. N replicas migrating at boot means
  N racing writers.
- The migrate path builds no access model, so it gets only `JDX_BACKEND` and
  `JDX_DSN`, not the identity secret.

Migration `0002` runs `CREATE EXTENSION IF NOT EXISTS vector`. On DO managed
Postgres, create the extension once as `doadmin` before the first migrate. The
migration's own statement is then a no-op.

## Dual health probes

- `GET /health/ready` is the **rotation** gate. It re-reads the projection head
  (`max(facts.seq)`, bounded at 2s) and answers 503 while the store is unreachable
  (jumpdrive-index#57).
- `GET /health/alive` is the **restart** gate. It checks only the process and never
  the store, so a database outage can't restart-loop the service.

## Required env

| Env | Type | Notes |
|-----|------|-------|
| `JDX_MODE` | plain | `serve`, or `migrate` for the migrate job |
| `JDX_BACKEND` | plain | `postgres` |
| `JDX_DSN` | **SECRET** | `postgresql://…/jumpdrive_index?sslmode=require&pool_max_conns=4` on the direct port, not a pooler, because migrations take advisory locks |
| `JDX_HTTP_ADDR` | plain | `:8090`. A routable bind, so `JDX_AUTH=true` is mandatory (ADR-0011 boot refusal) |
| `JDX_AUTH` | plain | `true` |
| `JDX_IDENTITY` | plain | `jumpdrive`. `starchart` is refused on Postgres unless `JDX_LEGACY_STARCHART=1` is set; no hosted index has ever needed that |
| `JDX_JUMPDRIVE_URL` | plain | `https://<console host>/internal/index`. The index fetches `<url>/jwks.json` and POSTs `<url>/authorize`. Use the public origin: the issuer is derived as this URL minus `/internal/index` and must equal the control plane's `JUMPDRIVE_BASE_URL` (`JDX_JUMPDRIVE_ISSUER` overrides it) |
| `JDX_JUMPDRIVE_VERIFY` | plain | `jwks` \| `authorize` \| `dual`. Use `dual` for rollout, `jwks` as the end state, and `authorize` as the incident fallback |
| `JDX_JUMPDRIVE_SECRET` | **SECRET** | the index→control-plane service bearer for `/authorize`, equal to the control plane's `JUMPDRIVE_INDEX_AUTHORIZE_SECRET`. Required with `JDX_IDENTITY=jumpdrive` in every verify mode |
| `JDX_HEYARR_URL` / `JDX_HEYARR_TOKEN` | plain / **SECRET** | optional, and only together: a token with no URL is a boot refusal |

## Changing the live app

The jumpdrive-web rules apply. Never `doctl apps update --spec` a committed spec:
committed SECRETs have no value, so applying one wipes the live secrets. Use
jumpdrive-web's `/do-live-spec` round-trip: fetch the live spec, edit only what you
need, and apply that.
