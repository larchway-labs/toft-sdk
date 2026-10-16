package toft

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The app acting for whoever is looking at the page.
//
//	me, err := toft.ViewerFor(r.Header)
//	run, err := me.Jobs.Start(ctx, "reconcile", toft.StartOptions{Params: params})
//	recent, err := me.Jobs.Mine(ctx, "reconcile", 20)
//
// **The app never asserts who the viewer is.** What it relays is a blob the
// platform signed over (user, app, session) on its way through the gateway. The
// app cannot mint one, cannot alter one, and cannot read who is inside it — which
// is the same contract X-Toft-Session already has: a correlation id, not
// identity.
//
// What falls out is the valuable part: the platform holds the real principal at
// the moment it decides. Whether this person may start this job is evaluated
// against *their* permissions, not against the app's claim about them, so an app
// cannot launder a viewer into authority they do not have. And it works in the
// other direction too — [JobsClient.Mine] answers a question *about* the viewer
// without disclosing the viewer.
//
// **A missing header is an error.** It never falls back to the app acting as
// itself: a silent downgrade would move a run out of a person's name and into
// nobody's, and nobody would notice until an audit. The usual cause is the app
// being called outside the Toft gateway — a health check, a direct port, a
// local dev server.
//
// **A visitor who is not signed in now relays a header too, and every call on
// the viewer it produces is refused.** Toft mints a blob that says *nobody
// in particular*, so [ViewerFor] succeeds and the platform then answers 403 with
// `viewer_anonymous`. Before this, a stranger relayed nothing — and relaying
// nothing means *the app acting as itself*, so work started for a stranger ran
// with the app's whole authority. That was never a decision anybody made; it was
// the only shape the system had. It is a change, and it is deliberate: a page
// that starts work "as the viewer where possible" should ask [Who] first, and
// use [Jobs] deliberately for the anonymous case if that is what it means.
//
// **It expires with the session it came from.** When it does, the person signs in
// again — nothing is broken.

// ActForHeader is the request header the Toft proxy sets on its way to the
// app.
const ActForHeader = "X-Toft-Act-For"

// IdentityTTL is how long a resolved identity is reused for the same blob.
//
// A page that re-renders per interaction must not become a request per render,
// and 60 seconds is the honest trade rather than a tuned number. What it buys and
// what it costs, plainly: **a role change takes effect in the app within the TTL,
// not instantly.** Where the *platform* enforces — starting a job, running a
// scheduled task — it re-resolves live and this cache is irrelevant. Where the
// *app* enforces, the app is the enforcer and the window is the app's to accept
// or to shorten. [WhoNow] bypasses it for the one call an app wants to be certain
// about.
const IdentityTTL = 60 * time.Second

// identityCacheMax bounds the cache as well as expiring it: an app serving many
// people would otherwise hold one entry per session for the life of the process.
// Evicting the whole thing when it fills is deliberate — an LRU here would be
// machinery in aid of a cache whose entries are worth 60 seconds each.
const identityCacheMax = 512

var (
	identityMu    sync.Mutex
	identityCache = map[string]cachedIdentity{}
)

type cachedIdentity struct {
	until time.Time
	who   *Identity
}

// Group is one group the viewer belongs to.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Identity is who is looking at this app, and what they may do *here*.
//
// Role answers one question and it is worth being exact about which: *what may
// this person do to this deployment in Toft* — redeploy it, edit its jobs,
// change its schedules. It is "owner", "editor" or "viewer", the same three words
// the Sharing tab granted from.
//
// It is **not** domain authority. App authors will reach for it as one, because
// it is the signal in front of them, and "owners may approve invoices" is a rule
// about invoices rather than about deployments. Groups is where a domain rule
// belongs — "finance" and "approvers" are the things that mean what they say.
//
// IsAdmin is separate from Role on purpose. Toft hands an administrator
// effective ownership of every app, so an app gating a destructive action on
// Role == "owner" alone would be granting it to every administrator on the
// install with no way to refuse. The common line is `me.Role == "owner" ||
// me.IsAdmin`; an app that means to withhold simply does not write the second
// half.
//
// Email is empty when the app's visibility is public. Not a failure and not a
// person without an address: a public app's visitors did not consent to a mailing
// list by visiting. `toft.App.Can(ctx, "viewer.identity.email")` says so at startup
// rather than leaving an author to infer it from a blank.
type Identity struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Email     string  `json:"email"`
	AvatarURL string  `json:"avatar_url"`
	Role      string  `json:"role"`
	IsAdmin   bool    `json:"is_admin"`
	Groups    []Group `json:"groups"`
}

// GroupNames are the group names alone, for the ordinary membership test.
func (i Identity) GroupNames() []string {
	out := make([]string, 0, len(i.Groups))
	for _, group := range i.Groups {
		out = append(out, group.Name)
	}
	return out
}

// InGroup reports whether the viewer belongs to the named group.
func (i Identity) InGroup(name string) bool {
	for _, group := range i.Groups {
		if group.Name == name {
			return true
		}
	}
	return false
}

// Viewer is one person, as a scoping capability rather than a fact the app holds.
//
// The capability half is the default: everything an app wants to do *with* a
// viewer — start work in their name, list what they started — the platform does
// on its behalf, holding the real principal at the moment it decides.
//
// [Viewer.Identity] is the deliberate exception, and it is a *question the app
// must answer for itself*: may this person see the approvals tab. The platform is
// not in the loop there, so the app is the enforcer and needs the fact. It is off
// until an administrator enables it for this app.
type Viewer struct {
	blob string

	// Jobs is the job surface with this viewer attached.
	Jobs *JobsClient
}

// String never renders the blob: it is a bearer credential for one session, and
// a log line is exactly where one should not end up.
func (v *Viewer) String() string {
	return "<toft.Viewer (a signed-in person; the app cannot see who)>"
}

// Identity resolves who this viewer is, or nil if there is nobody to name.
func (v *Viewer) Identity(ctx context.Context) (*Identity, error) {
	return resolveIdentity(ctx, v.blob, false)
}

// ViewerFor builds a [Viewer] from the request headers the app is serving.
//
// It fails when the act-for header is absent, naming it and the likely cause.
func ViewerFor(headers http.Header) (*Viewer, error) {
	blob := findHeader(headers, ActForHeader)
	if blob == "" {
		return nil, &Error{Message: "no " + ActForHeader + " header on this request, so there " +
			"is no viewer to act for. Toft's proxy sets it on every request from a " +
			"signed-in person; its absence usually means the app was reached outside the " +
			"gateway — a health check, a direct port, or a local development server. Use " +
			"toft.Jobs to act as the app itself, deliberately, rather than treating this as " +
			"the same thing."}
	}
	return &Viewer{blob: blob, Jobs: &JobsClient{headers: map[string]string{ActForHeader: blob}}}, nil
}

// Who is who is looking at this app, or nil if there is nobody to name.
//
//	me, err := toft.Who(ctx, r.Header)
//	if me != nil && (me.Role == "owner" || me.Role == "editor") {
//		renderAdminTab()
//	}
//	if me != nil && me.InGroup("finance") {
//		renderLedger()
//	}
//
// **This never fails for an absent viewer, and [ViewerFor] still does.** The two
// answer different questions — *who is this* and *act as this person* — and only
// the first one can honestly answer "nobody". A public app rendering "sign in to
// see your reports" should not have to treat the normal case as an error; an app
// about to start a job in somebody's name should never silently start it in
// nobody's.
//
// A nil identity with a nil error means nobody is there: an anonymous visitor, or
// somebody whose access was revoked while their page stayed open. **It never
// means "not allowed to ask"** — that is an error, naming the setting and saying
// an administrator enables it. An app that would rather never see that error
// checks `toft.App.Can(ctx, "viewer.identity")` at startup and never calls this on
// an install where it is off.
func Who(ctx context.Context, headers http.Header) (*Identity, error) {
	return who(ctx, headers, false)
}

// WhoNow is [Who] with the 60-second cache bypassed, for the one decision an app
// wants to be certain about.
func WhoNow(ctx context.Context, headers http.Header) (*Identity, error) {
	return who(ctx, headers, true)
}

func who(ctx context.Context, headers http.Header, refresh bool) (*Identity, error) {
	blob := findHeader(headers, ActForHeader)
	if blob == "" {
		return nil, nil
	}
	return resolveIdentity(ctx, blob, refresh)
}

// resolveIdentity asks the platform, through the cache.
//
// Keyed on the blob rather than on a user id, because the blob is all the app has
// and two people are never behind one — it is signed over (user, app, session).
func resolveIdentity(ctx context.Context, blob string, refresh bool) (*Identity, error) {
	now := time.Now()
	if !refresh {
		identityMu.Lock()
		hit, ok := identityCache[blob]
		identityMu.Unlock()
		if ok && hit.until.After(now) {
			return hit.who, nil
		}
	}

	identity, err := fetchIdentity(ctx, blob)
	if err != nil {
		return nil, err
	}

	identityMu.Lock()
	if len(identityCache) >= identityCacheMax {
		identityCache = map[string]cachedIdentity{}
	}
	identityCache[blob] = cachedIdentity{until: now.Add(IdentityTTL), who: identity}
	identityMu.Unlock()
	return identity, nil
}

func fetchIdentity(ctx context.Context, blob string) (*Identity, error) {
	t, err := client()
	if err != nil {
		return nil, err
	}
	raw, err := t.get(ctx, "/api/v1/app/viewer", map[string]string{ActForHeader: blob})
	if err != nil {
		return nil, err
	}
	// 204: the app's own credential was valid and the question was answerable.
	// The answer is that there is no viewer.
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var identity Identity
	if err := decode(raw, &identity); err != nil {
		return nil, err
	}
	return &identity, nil
}

// resetIdentityCache drops every cached identity. For tests; app code has
// [WhoNow].
func resetIdentityCache() {
	identityMu.Lock()
	identityCache = map[string]cachedIdentity{}
	identityMu.Unlock()
}

// findHeader reads one header, canonically first and case-insensitively after.
//
// http.Header.Get canonicalizes, which covers everything net/http parsed itself.
// The scan is for a header map an app built by hand — from a gRPC gateway, a
// framework's own type, a test — where the keys never went through
// textproto.CanonicalMIMEHeaderKey and a case-sensitive lookup would be a bug
// that only shows up on somebody else's stack.
func findHeader(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) {
			for _, value := range values {
				if value != "" {
					return value
				}
			}
		}
	}
	return ""
}
