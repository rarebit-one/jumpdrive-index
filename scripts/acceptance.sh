#!/usr/bin/env bash
# Acceptance: build the binary, migrate a fresh SQLite db, serve, and drive an
# end-to-end MCP-over-HTTP loop (initialize -> create_entity -> get_entity ->
# resolve_external) against the running process. This proves the whole stack —
# config -> store -> access -> service -> MCP -> HTTP — actually serves, which a
# unit test cannot claim about a live process. It is `make demo`'s gate.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
tmp="$(mktemp -d)"
srv=""
cleanup() { [ -n "$srv" ] && kill "$srv" 2>/dev/null || true; rm -rf "$tmp"; }
trap cleanup EXIT

addr="127.0.0.1:8791"
base="http://$addr"

echo "==> build"
CGO_ENABLED=0 go build -o "$tmp/jdx" ./cmd/jumpdrive-index

cat >"$tmp/principals.json" <<'JSON'
{"Principals":[{"Token":"demo-token","ID":"demo","Spaces":["home"],"ApproverSpaces":["home"]}]}
JSON

export JDX_BACKEND=sqlite JDX_DSN="$tmp/jdx.db" JDX_IDENTITY=starchart \
  JDX_PRINCIPALS_FILE="$tmp/principals.json" JDX_HTTP_ADDR="$addr" JDX_AUTH=true

echo "==> migrate"
JDX_MODE=migrate "$tmp/jdx"

echo "==> serve"
JDX_MODE=serve "$tmp/jdx" &
srv=$!

for _ in $(seq 1 60); do
  curl -fsS "$base/health/ready" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "$base/health/ready" >/dev/null || { echo "FAIL: server never became ready"; exit 1; }

mcp() { curl -fsS -X POST "$base/mcp" -H "Authorization: Bearer demo-token" -H "Content-Type: application/json" -d "$1"; }

echo "==> initialize"
mcp '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' | grep -q '2025-06-18' \
  || { echo "FAIL: initialize did not advertise the protocol version"; exit 1; }

echo "==> create_entity (Movie, tmdb:348)"
cr=$(mcp '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_entity","arguments":{"type":"Movie","props":{"name":"Alien"},"space":"home","visibility":"space","external_ids":[{"scheme":"tmdb","value":"348"}]}}}')
echo "$cr" | grep -q '"isError":false' || { echo "FAIL: create_entity: $cr"; exit 1; }
id=$(echo "$cr" | sed -n 's/.*"ID":"\([^"]*\)".*/\1/p' | head -1)
[ -n "$id" ] || { echo "FAIL: no entity id in $cr"; exit 1; }
echo "    id=$id"

echo "==> get_entity"
gt=$(mcp "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"get_entity\",\"arguments\":{\"id\":\"$id\"}}}")
echo "$gt" | grep -q '"Type":"Movie"'  || { echo "FAIL: get_entity type: $gt"; exit 1; }
echo "$gt" | grep -q '"Owner":"demo"'  || { echo "FAIL: owner not stamped from caller: $gt"; exit 1; }

echo "==> resolve_external tmdb:348"
mcp '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"resolve_external","arguments":{"keys":["tmdb:348"]}}}' \
  | grep -q "$id" || { echo "FAIL: resolve_external did not return the entity"; exit 1; }

echo "OK: end-to-end MCP-over-HTTP serve loop passed"
