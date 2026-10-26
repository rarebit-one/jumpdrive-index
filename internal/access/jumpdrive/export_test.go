package jumpdrive

// Wire types exposed to the contract drift gate (contract_test.go) only, so it
// can check that every key a vendored fixture carries is one this package
// decodes, and the reverse.
type (
	AuthorizeRequestWire  = authorizeRequest
	AuthorizeResponseWire = authorizeResponse
	AuthorizeErrorWire    = authorizeError
	JWSHeaderWire         = jwsHeader
	JWKWire               = jwk
)
