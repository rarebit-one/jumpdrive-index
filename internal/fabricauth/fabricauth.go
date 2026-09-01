// Package fabricauth builds the HTTP client Starchart uses to authenticate to the
// TechnoCore switchboard (Farcaster) with Voidbind's Device credential scheme,
// instead of a shared bearer. When the fabric runs FARCASTER_AUTH_MODE=voidbind,
// every caller must present `Authorization: Device <cert>~<possession>` — a fresh
// possession proof per request — which is exactly what voidbind-go's deviceclient
// RoundTripper does over an enrolled device.Store.
//
// Both fabric clients (the embedder and the ingest transcriber) accept an
// injectable http client, so wiring is a one-liner at construction: when a device
// directory is configured, hand them this client and drop the bearer.
package fabricauth

import (
	"fmt"
	"net/http"
	"time"

	"github.com/rarebit-one/voidbind-go/device"
	"github.com/rarebit-one/voidbind-go/deviceclient"
)

// Client returns an *http.Client that stamps a Voidbind Device credential on
// every request, loading Starchart's device identity from dir (a device store
// previously provisioned with `voidbind device generate` + `voidbind identity
// enrol`). timeout sets the client timeout.
//
// When dir is empty it returns (nil, nil): Voidbind is not configured, so the
// caller keeps its existing bearer/default client. An error is returned only when
// dir names a store that cannot be opened — a misconfiguration worth failing on,
// not silently falling back to unauthenticated calls.
func Client(dir string, timeout time.Duration) (*http.Client, error) {
	if dir == "" {
		return nil, nil
	}
	store, err := device.NewStore(device.StoreOptions{Dir: dir})
	if err != nil {
		return nil, fmt.Errorf("fabricauth: open device store %q: %w", dir, err)
	}
	c := deviceclient.Client(store)
	c.Timeout = timeout
	return c, nil
}
