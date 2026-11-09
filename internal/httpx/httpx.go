// Package httpx holds shared HTTP primitives used across this module.
package httpx

import "net/http"

// Doer is the minimal HTTP surface shared by all outbound callers. *http.Client
// satisfies it; tests inject a stub.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}
