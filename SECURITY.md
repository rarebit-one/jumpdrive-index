# Security policy

## Reporting a vulnerability

Please report security issues privately through
[GitHub Security Advisories](https://github.com/rarebit-one/jumpdrive-index/security/advisories/new)
rather than a public issue. We will acknowledge within 7 days.

jumpdrive-index is early and has had no security review. Do not expose it to an
untrusted network.

## Threat model

jumpdrive-index is a knowledge-graph index whose whole value is the access model,
so a few commitments shape what counts as a vulnerability:

- **Access is a hard boundary compiled into SQL, not a soft filter.** Every read
  is scoped by an `AccessFilter` that becomes a WHERE clause on nodes *and* edges.
  Any path by which a principal reads an entity or edge they are not granted —
  including *through* a hidden node during a multi-hop traversal — is a
  vulnerability, not a feature.
- **Edge visibility is independent of its endpoints.** "X relates to Y" can be
  more sensitive than either X or Y existing; a query that leaks a private edge
  between two visible nodes is a bug.
- **Child-safety is a restricted principal, never a lens.** A lens is a soft,
  droppable presentation filter. A boundary that a query can switch off is not a
  boundary; treat any such regression as a vulnerability.
- **Provenance is not authorization.** "Who asserted this" and "who may see this"
  are separate fields and must stay separate.
- **Deploy posture (Starchart).** The service refuses to serve unauthenticated on
  a routable (non-loopback) address. Bypassing that refusal is a vulnerability.

## Not in scope

Content that a deployment chooses to store, and correctness of third-party MCP
clients, are out of scope.
