# Starchart — reference Linux host

How to run **Starchart** (the self-hosted, single-tenant/SQLite build of
`jumpdrive-index`) host-native under systemd, hardened. Mirrors the shape of
heyarr's deployment; the crucial difference is Starchart owns all its own bytes
and mounts no external media library.

## FHS layout

| Path | What | Mode |
|------|------|------|
| `/usr/local/bin/jumpdrive-index` | the static binary | 0755 root:root |
| `/etc/starchart/starchart.env` | configuration (all `JDX_*` env) | 0640 root:starchart |
| `/etc/starchart/principals.json` | the access model (principals + tokens) | 0640 root:starchart |
| `/var/lib/starchart/starchart.db` | the SQLite database (the fact log + projection) | owned by `starchart` |
| `/var/lib/starchart/embeddings/` | vector artifacts (reserved) | owned by `starchart` |
| `/run/starchart/` | the unix socket (reserved) | 0700 |

`StateDirectory=starchart` in the unit creates and owns `/var/lib/starchart`
(systemd handles ownership — no run-once postinstall).

## Service account

```bash
sudo useradd --system --home-dir /var/lib/starchart --shell /usr/sbin/nologin starchart
```

No `media` group, no ACLs: Starchart reads no external library.

## Install

The reference host has **no Go toolchain** — cross-compile a static binary and
copy it over:

```bash
# on a build machine
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o jumpdrive-index ./cmd/jumpdrive-index
scp jumpdrive-index <host>:/tmp/

# on the host
sudo install -m0755 /tmp/jumpdrive-index /usr/local/bin/jumpdrive-index
sudo install -d -m0750 -o root -g starchart /etc/starchart
sudo install -m0640 -o root -g starchart deploy/systemd/starchart.env.example      /etc/starchart/starchart.env
sudo install -m0640 -o root -g starchart deploy/systemd/principals.example.json    /etc/starchart/principals.json
sudo install -m0644 deploy/systemd/starchart.service /etc/systemd/system/starchart.service
# edit /etc/starchart/principals.json — replace the CHANGE-ME tokens with long random secrets
sudo systemctl daemon-reload
sudo systemctl enable --now starchart
```

The unit's `ExecStartPre` runs `MODE=migrate` before each serve, so the database
is created/upgraded automatically on first start and on binary upgrades.

## Verify

```bash
# hardening — record the score, as heyarr does; removing any directive raises it
systemd-analyze verify starchart.service
systemd-analyze security starchart

# health + a smoke MCP call (loopback; use your real token)
curl -fsS http://127.0.0.1:8090/health/ready
curl -fsS -X POST http://127.0.0.1:8090/mcp \
  -H "Authorization: Bearer <your-token>" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'
```

`scripts/acceptance.sh` runs the same build → migrate → serve → MCP loop and is
the packaged smoke test.

## Auth posture (ADR-0011)

Starchart binds **loopback + auth by default**. It **refuses to start** if
configured to a routable (non-loopback) address with `JDX_AUTH` off. Put it
behind Tailscale or a reverse proxy for remote access and keep `JDX_AUTH=true`.

## Container alternative

`deploy/docker/Dockerfile` builds a distroless image (uid 65532). Same config via
`JDX_*` env; mount a volume at `/var/lib/starchart`; keep `JDX_AUTH=true` if you
publish a routable port.
