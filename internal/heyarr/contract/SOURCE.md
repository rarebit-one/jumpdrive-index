# heyarr MCP contract — vendored fixtures

These JSON fixtures pin the byte-for-byte shape of the heyarr MCP tools this
package depends on. They are the single source of truth for the drift gate in
`contract_test.go`: the `heyarr.Client` must emit the pinned request `params` and
decode the pinned response payload. They are **vendored** — never hand-edited to
follow the client; when heyarr's tool changes, re-capture from heyarr and update
the SHA below, and the test then reflects the real contract.

## Source

- Repo: `github.com/rarebit-one/heyarr-core`
- Commit: **`9443e4a`** (`feat(mcp): get_external_ids — read stored external identifiers (ADR-0050) (#355)`)
- Tool: `get_external_ids` (`ScopeRead`), defined in heyarr `internal/api/mcp/reads.go`
  (handler) + `schema.go` (input schema) + `tools.go` (registration).

## Contract — `get_external_ids`

- **Request** (`tools/call` params): `{"name":"get_external_ids","arguments":{…}}`,
  arguments = EITHER an entity ref (`work_id` | `edition_id`, forward) OR a
  `source`+`value` pair (reverse).
- **Response** (structured content): `{"external_ids":[{"source","value","entity_type","entity_id"}]}`,
  a uniform list, empty on no match (never an error for an absent id).

Fixtures:

- `get_external_ids.reverse.request.json` / `.response.json` — `source`+`value` → the carrier.
- `get_external_ids.forward.request.json` / `.response.json` — `work_id` → its ids.

## Re-vendoring

When heyarr changes `get_external_ids` (argument names, response fields), capture
the new request `params` and response `structuredContent` from heyarr's
`internal/api/mcp` (its `tools_test.go` / golden `testdata/tools_list.json` show
the schema; a live call shows the payload), overwrite the fixtures here, bump the
commit SHA above, and re-run `go test ./internal/heyarr/...`. A red drift test
means the client and heyarr have diverged — fix the client (or the fixtures) so
they agree, deliberately.
