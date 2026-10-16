package toft

import "sync"

// The process-wide transport, built on first use.
//
// One connection story for the whole SDK, so [App] and [Notify] cannot end up
// talking to two different places, and so importing this package never touches
// the environment. Nothing in normal app code calls [SetTransport]; it exists
// for tests and for an app that re-reads its own configuration.

var (
	sharedMu sync.Mutex
	shared   *transport
)

func client() (*transport, error) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if shared != nil {
		return shared, nil
	}
	built, err := newTransport()
	if err != nil {
		return nil, err
	}
	shared = built
	return shared, nil
}

// SetTransport points the SDK at a base URL and token of the caller's choosing,
// replacing whatever the environment said.
//
// For tests, and for the rare app that reloads its own configuration. Passing
// an empty base URL drops the built transport, so the next call reads the
// environment again.
func SetTransport(baseURL, token string) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if baseURL == "" {
		shared = nil
		return
	}
	shared = &transport{base: trimSlash(baseURL), token: token, client: noRedirectClient()}
}
