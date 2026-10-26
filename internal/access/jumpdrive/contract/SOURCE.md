# jumpdrive caller-identity contract — vendored fixtures

These files pin the byte-for-byte shape of the per-caller credential, the JWKS
and the `/authorize` exchange that the `jumpdrive` identity backend depends on.
The control plane owns them; they are the single source of truth for the drift
gate in `../contract_test.go`, which checks that the verifier and the authorize
client read exactly the keys the control plane sends, keep its pinned constants
(`alg`, `typ`, `aud`, `jd_ver`, `contract`), and can verify, decode and act on
each fixture. They are **vendored** — never hand-edited to follow the code; when
the control plane changes the contract, re-vendor from jumpdrive-web and update
the SHA below, and the test then reflects the real contract.

`README.md` is the control plane's own description of the files, vendored
unchanged with them.

## Source

- Repo: `github.com/rarebit-one/jumpdrive-web`
- Path: `control-plane/spec/fixtures/index_authorize_contract/`
- Commit: **`c5915a5f`** (`origin/main`; the fixtures last changed in `9cfa9fb7`,
  `feat(index): /internal/index/authorize + jwks.json, dark (#748, 2/3) (#1023)`)
- Design: jumpdrive-web `docs/index-principal-adr.md`, Decisions 2 and 3
  (ratified in jumpdrive-web#999).
- Control-plane gates: `spec/services/index_authorize_contract_spec.rb`
  (credential, JWKS) and `spec/requests/internal/index_authorize_spec.rb`
  (authorize request, response, errors).

## Contract (v1)

| File | Pins |
|---|---|
| `credential.v1.json` | The JWS header (`alg=ES256`, `kid`, `typ=jd-index+jwt`) and claim set (`iss`, `aud=jumpdrive-index`, `sub`, `jd_org`, `jd_ws`, `jd_access`, `jd_spaces{read,write,approve}`, `jd_client`, `jd_ver=1`, `jti`, `iat`/`nbf`/`exp`, `exp = iat + 60`). |
| `jwks.v1.json` | `GET /internal/index/jwks.json`: EC P-256 `use=sig` `alg=ES256` keys, `kid` = RFC 7638 thumbprint. |
| `authorize-request.v1.json` | `POST /internal/index/authorize` with `Authorization: Bearer <JDX_JUMPDRIVE_SECRET>`, `Content-Type: application/json`, a propagated `X-Request-Id`, body `{"bearer": …}`. |
| `authorize-response.v1.json` | The `200` decision (`contract=jumpdrive-authorize/v1`, read/write/approver sets, `expires_at`) and `Cache-Control: max-age=…, private`. |
| `authorize-errors.v1.json` | Every `401`/`403` error body `{"error": "<code>"}`; `invalid_service_secret` is the operator-misconfiguration code. |

## Re-vendoring

```bash
git -C ../jumpdrive-web fetch origin main
for f in README.md credential.v1.json jwks.v1.json \
         authorize-request.v1.json authorize-response.v1.json authorize-errors.v1.json; do
  git -C ../jumpdrive-web show origin/main:control-plane/spec/fixtures/index_authorize_contract/$f \
    > internal/access/jumpdrive/contract/$f
done
go test ./internal/access/jumpdrive/...
```

Bump the commit SHA above. A red drift test means the index and the control
plane have diverged: fix the code (or, upstream, the fixtures) so they agree,
deliberately. A change to the claim set or the response shape is a new version
(`jd_ver`, `contract`) on the control plane, never an in-place edit.
