# jumpdrive-index caller-identity contract (v1)

The control plane OWNS this contract (#748, `docs/index-principal-adr.md`
Decisions 2 and 3). jumpdrive-index vendors these files byte-for-byte under its
own drift gate — the mirror image of `spec/fixtures/index_contract/`, which the
control plane vendors FROM the index. Change them here first, then re-vendor
on the index side.

| File | What it pins |
|---|---|
| `credential.v1.json` | The per-caller credential (`IndexCredential.mint`): the JWS header and claim set. ES256, `typ=jd-index+jwt`, `aud=jumpdrive-index`, `exp = iat + 60`. `jd_client` is present only when the `/mcp` call had an OAuth client. |
| `jwks.v1.json` | `GET /internal/index/jwks.json`: one public EC P-256 key with `use=sig`, `alg=ES256` and an RFC 7638 thumbprint `kid` (`{"keys": []}` while `JUMPDRIVE_INDEX_SIGNING_KEY` is unset). |
| `authorize-request.v1.json` | `POST /internal/index/authorize`: service auth is `Authorization: Bearer <JDX_JUMPDRIVE_SECRET>`, matched against the control plane's `JUMPDRIVE_INDEX_AUTHORIZE_SECRET`, and the body is `{"bearer": "<credential>"}`. |
| `authorize-response.v1.json` | The `200` decision. `spaces` is the READ set and `write_spaces` / `approver_spaces` are new (absent means `[]`, so fail closed). Each set is the intersection of the credential's claim and a live re-resolution. `expires_at` is the credential's `exp`, and `Cache-Control: max-age=min(30, exp - now), private`. |
| `authorize-errors.v1.json` | Every error body, as `{"error": "<code>"}`. `401` means the service secret or the credential is bad; `403` means the credential is valid but the live decision is refused. |

The drift gates on this side are `spec/services/index_authorize_contract_spec.rb`
(credential, JWKS) and `spec/requests/internal/index_authorize_spec.rb`
(authorize request, response and errors). They mint and serve real values and
assert that those carry exactly these keys, with these value types. A bump to the claim set or the response shape is a new
version (`jd_ver`, `contract`), never an in-place edit.
