package toft

import (
	"context"
	"os"
	"strings"
	"sync"
)

// The app's view of itself.
//
// [App] answers two different kinds of question and it is worth keeping them
// apart.
//
// *Who am I* — id, slug, name, execution mode, base path — is for the app's own
// logs and for building links back to itself.
//
// *What may I do* — [AppClient.Can] and [AppInfo.Capabilities] — is the one that
// changes how an app should be written. An administrator attaches integrations
// to an app; app code that asks first can render "email is not enabled for this
// app; ask an administrator" on a settings screen at startup, instead of
// discovering it at 2am as a refusal in a log nobody is reading:
//
//	if ok, _ := toft.App.Can(ctx, "email"); ok {
//		toft.Notify.Email(ctx, toft.EmailMessage{To: recipients, Subject: subject, Text: body})
//	} else {
//		log.Print("email not attached to this app; report written to storage only")
//	}
//
// The answer is attached-or-not and never *why* not. The reason is an
// administrator's business, and an app is not a principal that gets told about
// the install's configuration — the same line /system/settings draws by 404ing
// to everyone who is not an administrator.
//
// The whole view is fetched once and cached for the life of the process,
// because it is read on hot paths and an administrator's change lands at the
// next deploy anyway. [AppClient.Refresh] re-reads it for a long-lived app that
// would rather notice sooner.

// AppInfo is this app, as the platform sees it.
type AppInfo struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Mode     string `json:"mode"` // "shared" or "isolated"
	BasePath string `json:"base_path"`

	// Policy is the read-only block of rules this app is judged by: version
	// retention, whether it may sleep and why. The same block GET /apps/{id}
	// returns, for the same reason it exists there — a platform decision about
	// one app is visible on that app.
	Policy map[string]any `json:"policy"`

	// Capabilities are every capability an administrator attached, e.g.
	// "email.send".
	Capabilities []string `json:"capabilities"`
}

// AppClient reads the app's own view of itself, once.
type AppClient struct {
	mu     sync.Mutex
	cached *AppInfo
}

// Self is the whole view: identity and capabilities together.
func (a *AppClient) Self(ctx context.Context) (*AppInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cached != nil {
		return a.cached, nil
	}
	t, err := client()
	if err != nil {
		return nil, err
	}
	raw, err := t.get(ctx, "/api/v1/app/self", nil)
	if err != nil {
		return nil, err
	}
	var info AppInfo
	if err := decode(raw, &info); err != nil {
		return nil, err
	}
	// BASE_PATH is the fallback, never the override: an install older than the
	// payload's base_path field left the key out, this reported "" as fact, and
	// an app building its links from it emitted root-absolute URLs — which the
	// proxy answers with a 404 on every link, with nothing in any log saying
	// why. "" is also the honest answer on an install that gives each app its
	// own origin, so nothing an app could check told the two apart.
	//
	// The variable is the value this same deploy injected into this process, so
	// it cannot disagree with the platform; it only answers where the platform
	// said nothing.
	if info.BasePath == "" {
		info.BasePath = strings.TrimSpace(os.Getenv(EnvBasePath))
	}
	a.cached = &info
	return a.cached, nil
}

// Refresh drops the cached view; the next read re-fetches it.
func (a *AppClient) Refresh() *AppClient {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cached = nil
	return a
}

// Can reports whether this app holds a capability.
//
// Takes either a full capability ("email.send") or an integration name
// ("email"), which is true when *any* of that integration's actions is
// attached. Both spellings are accepted because both are the obvious thing to
// type, and guessing wrong should not be a silent false.
func (a *AppClient) Can(ctx context.Context, capability string) (bool, error) {
	if capability == "" {
		return false, nil
	}
	info, err := a.Self(ctx)
	if err != nil {
		return false, err
	}
	prefix := capability + "."
	for _, held := range info.Capabilities {
		if held == capability || strings.HasPrefix(held, prefix) {
			return true, nil
		}
	}
	return false, nil
}
