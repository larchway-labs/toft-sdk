package toft

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// A file of its own rather than a part of app.go: what this reads is
// /api/v1/app/url, and the guard in sdk/appself_contract_test.go holds every
// JSON key in app.go to what /api/v1/app/self sends.

// URLTTL is how long an answer from /api/v1/app/url is reused. Short, because
// the address moves without a deploy; long enough that a job mailing a
// thousand people asks once.
const URLTTL = 60 * time.Second

// urlClock is the clock URL reads, swapped by tests.
var urlClock = time.Now

type appAddress struct {
	PublicURL string `json:"public_url"`
	BasePath  string `json:"base_path"`
}

var urlScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)

// URL is the absolute address of a page in this app, for a link that leaves it
// — an email, a Slack post, a PDF, a ticket. Inside the app's own pages,
// relative links built from [AppInfo.BasePath] stay the right tool.
//
//	toft.App.URL(ctx, "/reports/42")  // https://apps.example.com/apps/reporter/reports/42
//	toft.App.URL(ctx, "")             // the app's front page
//	toft.App.URL(ctx, run.StatusURL)  // a run's status page
//
// The address is asked of the platform rather than stored, and reused for
// [URLTTL]: an administrator can move the install or give the app a hostname of
// its own while it runs. Works the same in a request, a job or a schedule. If
// the platform cannot be reached the last address this process was told is
// used; with none, the error is returned. An install with no address
// configured refuses with no_public_address.
func (a *AppClient) URL(ctx context.Context, path string) (string, error) {
	if err := checkAppPath(path); err != nil {
		return "", err
	}
	addr, err := a.currentAddress(ctx)
	if err != nil {
		return "", err
	}
	return joinAppURL(addr.PublicURL, addr.BasePath, path)
}

func (a *AppClient) currentAddress(ctx context.Context) (*appAddress, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.address != nil && urlClock().Sub(a.addressAt) < URLTTL {
		return a.address, nil
	}
	t, err := client()
	if err != nil {
		return nil, err
	}
	raw, err := t.get(ctx, "/api/v1/app/url", nil)
	if err != nil {
		// Unreachable — no status, no code — is the one failure a known
		// address outlives. A refusal is the platform's answer and stands.
		var te *Error
		if a.address != nil && errors.As(err, &te) && te.Status == 0 && te.Code() == "" {
			return a.address, nil
		}
		return nil, err
	}
	var addr appAddress
	if err := decode(raw, &addr); err != nil {
		return nil, err
	}
	a.address, a.addressAt = &addr, urlClock()
	return a.address, nil
}

func checkAppPath(path string) error {
	if urlScheme.MatchString(path) || strings.HasPrefix(path, "//") {
		return badRequest(fmt.Sprintf("URL was given an absolute URL (%q), which it would join onto this app's address", path),
			`pass the path inside the app, e.g. toft.App.URL(ctx, "/reports/42")`)
	}
	return nil
}

// joinAppURL joins an app path onto the app's public address — [AppClient.URL]'s
// rule, held to sdk/testdata/app-url-vector.json with the other three SDKs. A
// path is the app's, so "/reports/42" and "reports/42" both mean the app's
// route, never the host's root. One that already carries the base path — a
// run's StatusURL, a framework's URL helper — is not prefixed twice. A query or
// fragment is kept. An absolute URL is refused.
func joinAppURL(publicURL, basePath, path string) (string, error) {
	if err := checkAppPath(path); err != nil {
		return "", err
	}
	if basePath != "" && strings.HasPrefix(path, basePath) {
		if rest := path[len(basePath):]; rest == "" || strings.ContainsAny(rest[:1], "/?#") {
			path = rest
		}
	}
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	return strings.TrimRight(publicURL, "/") + path, nil
}
