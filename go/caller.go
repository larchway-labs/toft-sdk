package toft

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// What the caller in front of this request may do, in one word.
//
//	func handler(w http.ResponseWriter, r *http.Request) {
//		c := toft.CallerFor(r.Header)
//		if !c.AtLeast(toft.RoleEditor) {
//			http.Error(w, "not for you", http.StatusForbidden)
//			return
//		}
//		…
//	}
//
// Toft puts a signed role on every request it forwards, so an app that
// wants to protect an expensive endpoint no longer has to choose between
// exposing it to everyone who can open the page and asking the platform who the
// viewer is. The three person-words are the ones the Sharing tab granted from —
// "owner", "editor", "viewer" — plus "anonymous" for a visitor who is not signed
// in, "system" for Toft firing a scheduled task, and "app" for the app's own
// credential.
//
// **A role is not an identity and it is not domain authority.** It says what
// this caller may do to *this deployment in Toft* — redeploy it, edit its
// jobs, change its schedules. "Owners may approve invoices" is a rule about
// invoices, and belongs on a group: see [Identity] and [Who]. Nothing here names
// a person, and nothing here is a substitute for [Who] when the question is who.
//
// **This is the app's decision, not the platform's.** Toft enforces nothing
// new: it tells you, you refuse. Where the *platform* decides — starting a job,
// running a scheduled task — it re-resolves live and never reads this header.
//
// **A missing or unverifiable header is [RoleUnknown], and [Caller.AtLeast] is
// false for it.** Absent key, absent header, bad signature and expired all give
// one answer, deliberately: four branches would be four ways to get a refusal
// wrong, and a verification whose failure modes were distinguishable would be an
// oracle. The one deployment where the header is missing — a health check, a
// direct port, a laptop — is the one nobody configured, so it must refuse.
//
// **Verify on arrival.** The assertion is about one request and is good for two
// minutes. For an ordinary handler that is the same moment. For a WebSocket the
// headers arrive at the handshake and the connection lasts hours, so read the
// role at the handshake and hold your own decision from there — the role word is
// from the moment the request was forwarded, and a change of role during a
// long-held connection does not reach you. The platform's own enforcement is
// unaffected; the app's window is the app's, the same trade [IdentityTTL]
// documents.

// RoleHeader is the request header the Toft proxy sets on every request it
// forwards, and on every scheduled firing it makes.
const RoleHeader = "X-Toft-Role"

// EnvRoleKey names the environment variable Toft injects the verification
// key into. Per app and per start: a redeploy replaces it, which is why nothing
// signed for the previous release verifies against this one.
const EnvRoleKey = "TOFT_ROLE_KEY"

// The seven words. Six arrive on the wire; [RoleUnknown] never does.
const (
	// RoleUnknown is every failure: no header, no key, a bad signature, a blob
	// for another app, an expired one. It is never privileged.
	RoleUnknown = "unknown"
	// RoleAnonymous is a visitor who is not signed in to Toft. Only a
	// public app sees it.
	RoleAnonymous = "anonymous"
	// RoleViewer, RoleEditor and RoleOwner are what this person may do to this
	// deployment. A Toft administrator with no grant on the app reads as
	// RoleViewer — the header carries no admin bit, and an app that needs to
	// know is asking about a person, which is what [Who] is for.
	RoleViewer = "viewer"
	RoleEditor = "editor"
	RoleOwner  = "owner"
	// RoleSystem is Toft acting on its own: today, a scheduled task firing
	// on its schedule. A *manual* firing of the same schedule carries the role
	// of whoever pressed the button.
	RoleSystem = "system"
	// RoleApp is the app's own credential — an app key presented at the
	// gateway, and an app-started job's callback.
	RoleApp = "app"
)

// roleVersion prefixes every blob. A version this SDK does not know is refused
// rather than parsed leniently — the fields are positional, and a future format
// misread as this one is how a verifier comes to answer confidently about the
// wrong thing.
const roleVersion = "v1"

// Caller is who is asking, as a role rather than a person.
type Caller struct {
	// Role is one of the seven constants above, and never anything else — a
	// word this SDK does not know is [RoleUnknown], so an install running ahead
	// of an app's SDK refuses rather than admitting a value the app has never
	// branched on.
	Role string
}

// CallerFor reads the role off the request headers the app is serving.
//
// Never fails. Off-platform, or on a request that did not come through the
// gateway, the role is [RoleUnknown] and every [Caller.AtLeast] is false.
func CallerFor(headers http.Header) Caller {
	return Caller{Role: verifyRole(
		os.Getenv(EnvRoleKey), findHeader(headers, RoleHeader), time.Now())}
}

// AtLeast reports whether this caller holds at least the given role on the
// person ladder: anonymous < viewer < editor < owner.
//
// **It answers only about the ladder.** [RoleSystem] and [RoleApp] are not on
// it and are false for every rung, because they are not more or less authorized
// than a person — they are a different kind of caller, and ranking Toft's
// scheduler above an owner would be a guess this SDK has no business making. An
// endpoint that serves both a person and its own schedule says so:
//
//	if c.Role != toft.RoleSystem && !c.AtLeast(toft.RoleEditor) { … refuse … }
//
// [RoleUnknown] is false for everything, including [RoleAnonymous] — an
// unverifiable caller is not "at least a stranger", it is nobody this app can
// say anything about.
//
// **A rung does not mean a human.** An app key may be minted carrying `viewer`
// or `editor`, by an owner or editor of this app who chose to describe their
// machine client that way, and such a key satisfies AtLeast exactly as a person
// with the same word does. That is the point of the word — the caller may do
// what it says — and it is why nothing here answers "is a human in front of
// me". See [Caller.IsPerson].
func (c Caller) AtLeast(role string) bool {
	mine, ok := personRank(c.Role)
	want, wantOK := personRank(role)
	return ok && wantOK && mine >= want
}

// IsPerson reports whether this caller is *on the person ladder* — anonymous,
// viewer, editor or owner. False for [RoleSystem], [RoleApp] and [RoleUnknown].
//
// **It does not mean a human, and it never checked for one.** This has always
// been membership of the ladder and nothing more; what changed is that a rung
// can now be reached by something other than a person, because an app key may
// be minted carrying `viewer` or `editor`. Before that, the two coincided by
// accident: no machine credential could reach the ladder at all.
//
// **This platform cannot tell you a human is in front of you.** Nothing here
// can, and nothing here pretends to — the honest question an app can ask is
// what the caller may do, which is what the word says.
func (c Caller) IsPerson() bool {
	_, ok := personRank(c.Role)
	return ok
}

// IsSystem reports whether this request is Toft firing a scheduled task on
// its schedule. A manual firing of the same schedule is the person who pressed
// the button, not this.
func (c Caller) IsSystem() bool { return c.Role == RoleSystem }

// SignedIn reports whether this caller is *above anonymous on the person
// ladder*. False for an anonymous visitor, and false for [RoleSystem],
// [RoleApp] and [RoleUnknown].
//
// Like [Caller.IsPerson], it is a fact about the ladder rather than about a
// human: an app key minted with `viewer` or `editor` is true here, and no
// account was signed into to produce it.
//
// This is the one bit a public app gains that it did not have: it can tell a
// reader with a rung from a stranger. It names nobody — but a log line per request
// would build a record of which of your readers have Toft accounts, which
// is worth deciding about rather than doing by default.
func (c Caller) SignedIn() bool {
	rank, ok := personRank(c.Role)
	return ok && rank > 0
}

// String is a debugging aid and is safe to log: it carries a role, never a
// person and never the blob.
func (c Caller) String() string { return "<toft.Caller " + c.Role + ">" }

func personRank(role string) (int, bool) {
	switch role {
	case RoleAnonymous:
		return 0, true
	case RoleViewer:
		return 1, true
	case RoleEditor:
		return 2, true
	case RoleOwner:
		return 3, true
	}
	return 0, false
}

// verifyRole is the app side of the platform's mint, and the two are one
// specification: v1.<role>.<base64url app id>.<expiry>.<mac>, where the MAC is
// HMAC-SHA256 over "toft-role\x00" plus the payload, keyed with
// TOFT_ROLE_KEY, base64url without padding.
//
// The app id is not checked here and does not need to be: the key is derived
// per app, so a blob minted for another app fails the MAC. Checking it as well
// would need this process to know its own id, which is a second thing to get
// wrong for no property gained.
func verifyRole(key, blob string, now time.Time) string {
	if key == "" || blob == "" {
		return RoleUnknown
	}
	idx := strings.LastIndexByte(blob, '.')
	if idx < 0 {
		return RoleUnknown
	}
	payload, sig := blob[:idx], blob[idx+1:]

	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("toft-role\x00"))
	mac.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	// Constant time, and before anything is parsed: structure decoded ahead of
	// authentication is how a verifier grows a second entry point.
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return RoleUnknown
	}

	fields := strings.Split(payload, ".")
	if len(fields) != 4 || fields[0] != roleVersion {
		return RoleUnknown
	}
	unix, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || !time.Unix(unix, 0).After(now) {
		return RoleUnknown
	}
	// A word this SDK does not know is unknown rather than passed through. The
	// app holds this key and could sign anything with it; app code switching on
	// six values must not be handed a seventh, whoever minted it.
	if _, ok := personRank(fields[1]); ok || fields[1] == RoleSystem || fields[1] == RoleApp {
		return fields[1]
	}
	return RoleUnknown
}
