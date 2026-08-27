// Package secret holds a redacting string wrapper for credentials. A Value keeps
// a token out of every default formatting path — logs, error strings, %v/%s/%#v
// dumps — so a bearer cannot leak by accident; the real bytes are handed out only
// at the point of use, via Reveal. It is the analogue of the plan's secret.Value.
package secret

// Value is a credential string that redacts itself in every default formatting
// path (String, GoString), so it cannot be logged or printed by accident. Read
// the underlying secret with Reveal, only where it is actually sent.
type Value string

// redacted is what a non-empty secret renders as in any formatting context.
const redacted = "[REDACTED]"

// String redacts under fmt's %v / %s and log output. An empty secret renders
// empty (there is nothing to hide), so "unset" stays visible in diagnostics.
func (v Value) String() string {
	if v == "" {
		return ""
	}
	return redacted
}

// GoString redacts under %#v, so a struct dump cannot leak the secret either.
func (v Value) GoString() string { return v.String() }

// Reveal returns the underlying secret. Call it only at the point of use (e.g.
// setting an Authorization header), never when logging or formatting.
func (v Value) Reveal() string { return string(v) }

// IsZero reports whether the secret is unset.
func (v Value) IsZero() bool { return v == "" }
