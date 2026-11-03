package toft

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Ephemeral scratch storage.
//
// Toft gives every app a scratch directory — TOFT_SCRATCH_DIR, with
// TMPDIR pointed at the same place — that is wiped on every start, restart, and
// redeploy. It is per-app, never backed up, and never shared with another app:
// the right home for uploaded files being processed, intermediate results, and
// anything else that must not outlive the running app. Durable data belongs on a
// mapped storage resource ([Storage]), not here.
//
// A Go app serving several people at once needs [SessionScratchDir]: the proxy
// sends an opaque per-login id in the X-Toft-Session header, and this turns
// it into a directory, so two browser logins working in the same app do not
// collide.
//
// **Anonymous viewers carry no header, so the app mints its own id** and hands
// it back in a cookie of its own. Substituting a constant — "anon", or any other
// fixed string — is the one thing this must not do: it puts every anonymous
// viewer of a public app in the same directory, where one viewer's upload
// overwrites another's and the analysis reads back the wrong file:
//
//	const viewerCookie = "viewer_sid" // the app's own cookie; never a platform name
//
//	func upload(w http.ResponseWriter, r *http.Request) {
//		sid := r.Header.Get(toft.SessionHeader)
//		if sid == "" {
//			if c, err := r.Cookie(viewerCookie); err == nil {
//				sid = c.Value
//			}
//		}
//		if sid == "" {
//			sid, _ = toft.NewSessionID()
//			http.SetCookie(w, &http.Cookie{
//				Name: viewerCookie, Value: sid, HttpOnly: true,
//				SameSite: http.SameSiteLaxMode, Secure: true,
//			})
//		}
//		dir, err := toft.SessionScratchDir(sid)
//		...
//	}
//
// Header first, then the app's cookie: a viewer who signs in part-way through
// moves to the login's directory and leaves the anonymous one behind, which is
// the right way round — anonymous work does not follow somebody into their
// account.
//
// The platform id is per browser login, not per tab: two tabs in one browser
// share the login cookie and therefore the directory; two browsers or devices
// signed in as the same person do not. An app with a finer-grained session id of
// its own (a websocket connection, say) can pass that instead — the argument is
// any app-chosen string.
//
// The per-session split is a convention inside one app process, not a security
// boundary: all sessions run under one OS identity, and the app's own code can
// list every session's directory. Anything needing a real boundary between
// viewers should not be keyed on this id.

// EnvScratchDir names the app's scratch root.
const EnvScratchDir = "TOFT_SCRATCH_DIR"

// SessionHeader carries the viewer's opaque per-login session id.
const SessionHeader = "X-Toft-Session"

// ScratchDir is the app's scratch directory, created if missing.
//
// Optional parts name a subpath, which is created too. Outside Toft the
// system temp directory stands in, so app code runs unchanged in local
// development — the one call in this package that does not need the platform.
//
// The parts stay under the scratch root: an absolute part, or one whose ".."
// segments climb out of it, is refused bad_request rather than joined — the
// directory is the app's, and "../escape" would otherwise land in the app's own
// tree beside its code.
func ScratchDir(parts ...string) (string, error) {
	root := os.Getenv(EnvScratchDir)
	if root == "" {
		local, err := localScratchRoot()
		if err != nil {
			return "", err
		}
		root = local
	}
	path, err := underScratch(filepath.Clean(root), "ScratchDir", parts)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", &Error{Body: refusal("scratch_unavailable"), Message: fmt.Sprintf("cannot create the scratch directory %s: %v", path, err), Wrapped: err}
	}
	return path, nil
}

// localScratchRoot is off-platform scratch: <tmp>/toft-scratch-<uid>/<app>,
// 0700 — as the Python and Node SDKs make it. Never the shared temp directory
// itself, where every user on the machine can read what an app wrote and plant
// a file or symlink under a name it will use. Per user, so a directory another
// user created first is not adopted; per app (keyed on the working directory),
// so two apps in development do not share one. A parent that is not a private
// directory of this user's is passed over for a fresh MkdirTemp.
func localScratchRoot() (string, error) {
	localScratch.Lock()
	defer localScratch.Unlock()
	if localScratch.path != "" {
		if fi, err := os.Lstat(localScratch.path); err == nil && fi.IsDir() {
			return localScratch.path, nil
		}
	}
	cwd, _ := os.Getwd()
	sum := sha256.Sum256([]byte(cwd))
	parent := filepath.Join(os.TempDir(), fmt.Sprintf("toft-scratch-%d", os.Getuid()))
	root := filepath.Join(parent, hex.EncodeToString(sum[:])[:12])
	ok := true
	for _, dir := range []string{parent, root} {
		if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
			ok = false
			break
		}
		if !privateDir(dir) {
			ok = false
			break
		}
	}
	if !ok {
		tmp, err := os.MkdirTemp("", "toft-scratch-")
		if err != nil {
			return "", &Error{Body: refusal("scratch_unavailable"), Message: fmt.Sprintf("cannot create a private scratch directory: %v", err), Wrapped: err}
		}
		root = tmp
	}
	localScratch.path = root
	return root, nil
}

var localScratch struct {
	sync.Mutex
	path string
}

// underScratch joins parts under base, refusing an absolute part or one whose
// ".." segments climb out of base.
func underScratch(base, call string, parts []string) (string, error) {
	for _, part := range parts {
		if filepath.IsAbs(part) || strings.HasPrefix(part, "/") || strings.HasPrefix(part, `\`) {
			return "", &Error{Body: refusal("bad_request", "pass a path relative to the scratch directory"),
				Message: fmt.Sprintf("%s takes parts relative to the scratch directory, got the absolute %q", call, part)}
		}
	}
	path := filepath.Join(append([]string{base}, parts...)...)
	if rel, err := filepath.Rel(base, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &Error{Body: refusal("bad_request", "pass a path that stays under the scratch directory, with no '..' climbing out of it"),
			Message: fmt.Sprintf("%s(%q) resolves to %s, outside %s", call, strings.Join(parts, "/"), path, base)}
	}
	return path, nil
}

// NewSessionID mints a fresh opaque session id, for a viewer the platform
// cannot name.
//
// Anonymous and API-key requests carry no X-Toft-Session, because there is
// no server-side session to derive one from. An app that still wants per-viewer
// scratch mints one here and carries it in a cookie of its own, checked after
// the header and before this.
//
// Unguessable on purpose. The directory name is a hash of this string, so anyone
// who can guess an id can name another viewer's directory — which is why this is
// crypto/rand and 128 bits rather than a counter or a timestamp.
//
// Two rules for the cookie that carries it:
//
//   - **Never a name under the toft_ prefix** — that prefix is the
//     platform's, and every cookie under it is stripped from a request before the
//     app sees it, so an id stored there never survives a round trip and every
//     request would silently mint another. Every other name, "session" included,
//     is the app's own.
//   - HttpOnly, SameSite=Lax, and Secure wherever the app is served over https.
func NewSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", &Error{Body: refusal("entropy_unavailable"), Message: fmt.Sprintf("cannot mint a session id: %v", err), Wrapped: err}
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// SessionScratchDir is a per-session directory under [ScratchDir], created if
// missing.
//
// sessionID is whatever the app read from the X-Toft-Session header — or
// any string of its own choosing, such as a framework's finer per-connection
// session id. It is hashed before it becomes a directory name, so a malformed or
// hostile value cannot traverse outside the scratch root. An empty or
// whitespace-only id is refused bad_request.
func SessionScratchDir(sessionID string, parts ...string) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", &Error{Body: refusal("bad_request", "pass r.Header.Get(toft.SessionHeader), or an id from toft.NewSessionID() for a viewer with no session"), Message: "SessionScratchDir needs a non-empty session id — an " +
			"empty one would silently collapse every session into the same directory"}
	}
	sum := sha256.Sum256([]byte(sessionID))
	encoded := hex.EncodeToString(sum[:])[:32]
	// The parts are held to the session's own directory, not just the root: a
	// "../" from one session's directory is another session's.
	if _, err := underScratch(encoded, "SessionScratchDir", parts); err != nil {
		return "", err
	}
	return ScratchDir(append([]string{encoded}, parts...)...)
}

// privateDir is a real directory (not a symlink), this user's, with no group or
// other access.
func privateDir(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return ownedByMe(fi) && fi.Mode().Perm()&0o077 == 0
}
