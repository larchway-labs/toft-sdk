package toft

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Durable storage, resolved by name.
//
// TOFT_STORAGE is a JSON array of the resources an administrator mapped to
// this app. App code resolves a resource by the **name** it was given, never by
// the path it happens to be mounted at today — that is what lets an
// administrator move a resource without every app that uses it breaking, and
// hard-coding "/data/reports" is exactly what this exists to stop:
//
//	reports, err := toft.Storage("reports")
//	os.WriteFile(filepath.Join(reports.Path, "2026-08.xlsx"), data, 0o644)
//
// A miss names what *is* mounted. "no such resource" on its own sends somebody
// to their administrator to ask a question the process could already answer.

// Mount is one storage resource mapped to this app.
type Mount struct {
	Name   string
	Kind   string
	Path   string
	Access string
	// Folder is which directory of the resource this mount is rooted at, and
	// "" is the whole resource. It is here because **two mounts of one resource
	// share a name**, and the name is how app code locates a resource — so this
	// is what tells them apart. Deliberately not part of Path: where a mount
	// appears stays the administrator's choice.
	Folder string
}

// ReadOnly reports whether an administrator mapped this resource read-only.
func (m Mount) ReadOnly() bool { return m.Access == "read" }

// Mounted reports whether this resource is on this workload's filesystem at
// all. A store on a shared host is not: its Path is "", and joining a name onto
// "" writes into the app's working directory — so check this, or use the
// methods in store.go, which reach a store the same way in both modes.
func (m Mount) Mounted() bool { return m.Path != "" }

// Put copies a local file into this mount, and returns where it landed.
//
// Sugar over the mounted path and nothing more:
//
//	toft.Store("reports").Put("nightly.parquet", "2026-08.parquet")
//
// An app that has the store mounted already has the folder on its filesystem;
// this saves it from joining paths, and calls nothing. Where the store is not
// mounted — on an install with no cluster — this refuses, and [Mount.Upload]
// reaches it through the platform instead, at the same level, under the same
// mapping.
//
// The access level is checked here only for a better message. **The mount is the
// enforcement**: Mountpoint refuses an unsupported operation, so an app that
// tries to replace an object it was granted write on fails at the first wrong
// call rather than corrupting data slowly.
func (m Mount) Put(source, name string) (string, error) {
	target := name
	if target == "" {
		target = filepath.Base(source)
	}
	if err := checkRelativeName(target); err != nil {
		return "", err
	}
	if !m.Mounted() {
		return "", &Error{Body: refusal("not_mounted", "use Upload, which reaches the store through the platform"), Message: fmt.Sprintf(
			"%q is not mounted here, so Put has nowhere to copy to — use Upload, which "+
				"reaches it through the platform", m.Name)}
	}
	if m.ReadOnly() {
		return "", &Error{Body: refusal("insufficient_access", "ask an administrator for `write` access where the resource is mapped"), Message: fmt.Sprintf(
			"%q is mounted read-only, so this app cannot write to it. An administrator "+
				"sets the access level where the resource is mapped.", m.Name)}
	}

	destination := filepath.Join(m.Path, filepath.FromSlash(target))
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", &Error{Message: err.Error(), Wrapped: err}
	}
	in, err := os.Open(source)
	if err != nil {
		return "", &Error{Message: fmt.Sprintf("cannot read %s: %v", source, err), Wrapped: err}
	}
	defer in.Close()
	out, err := os.Create(destination)
	if err != nil {
		return "", &Error{Message: fmt.Sprintf("cannot write %s: %v", destination, err), Wrapped: err}
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return "", &Error{Message: fmt.Sprintf("cannot write %s: %v", destination, err), Wrapped: err}
	}
	if err := out.Close(); err != nil {
		return "", &Error{Message: fmt.Sprintf("cannot write %s: %v", destination, err), Wrapped: err}
	}
	return destination, nil
}

// checkRelativeName refuses a name that would land outside the mount.
//
// The same discipline the platform applies to an uploaded object's name, for the
// same reason: the name is the caller's and the location is not, so a traversal
// segment here would write into the app's own filesystem — over its code, in the
// worst case — rather than into the storage it was given.
func checkRelativeName(name string) error {
	refuse := func(format string, args ...any) error {
		return &Error{Body: refusal("invalid_name", nameHint), Message: fmt.Sprintf(format, args...)}
	}
	switch {
	case name == "":
		return refuse("a name is required")
	case strings.HasPrefix(name, "/"), strings.HasSuffix(name, "/"):
		return refuse("a name is relative to the mount, got %q", name)
	case strings.Contains(name, `\`):
		return refuse("a name must use forward slashes, got %q", name)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return refuse("a name must not contain %q segments, got %q", segment, name)
		}
	}
	return nil
}

// Mounts is every resource mapped to this app, in the order the platform listed
// them.
//
// That order is the platform's ORDER BY m.path, which is worth knowing only
// because two mounts can share a name: it is stable, and it is not a ranking.
// Ask for the one you mean with [StorageIn].
//
// Empty off-platform and empty when an administrator has mapped nothing —
// deliberately not an error, because "this app has no durable storage" is a
// normal state that app code can branch on.
func Mounts() ([]Mount, error) {
	raw := strings.TrimSpace(os.Getenv(EnvStorage))
	if raw == "" {
		return nil, nil
	}
	var entries []struct {
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Path   string `json:"path"`
		Access string `json:"access"`
		Folder string `json:"folder"`
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, &Error{Message: fmt.Sprintf("%s is not a JSON array of resources: %v", EnvStorage, err), Wrapped: err}
	}
	out := make([]Mount, 0, len(entries))
	for _, entry := range entries {
		out = append(out, Mount(entry))
	}
	return out, nil
}

// Storage resolves one resource by the name an administrator gave it.
//
// **A name is not always one mount.** A mapping's identity is (resource, app,
// folder), so an administrator can map one resource twice: the root read-only
// and a folder at `write`, at two paths in one workload. Both arrive under the
// same name, and the folder is what tells them apart.
//
// So a name matching two mounts **is an error here rather than a first match**.
// Returning the first was the earlier behaviour and it made the choice by the
// alphabetical order of paths an administrator chose — a silent answer to a
// question the caller did not know they were asking. [StorageIn] names one.
func Storage(name string) (Mount, error) { return storageIn(name, nil, "Storage") }

// StorageIn resolves one resource by name and folder, where it is mapped more
// than once. The empty string is the root mapping, named explicitly.
//
// A second function rather than an option struct: Go has no keyword arguments,
// and a struct carrying one string reads worse at every call site than the name
// does.
func StorageIn(name, folder string) (Mount, error) { return storageIn(name, &folder, "Storage") }

// storageIn is the resolver behind both accessors. accessor is the one the
// caller used — "Storage" or "Store" — so a refusal names the call to make
// rather than the other one (drills D17: Store("x") was told StorageIn).
func storageIn(name string, folder *string, accessor string) (Mount, error) {
	if name == "" {
		return Mount{}, &Error{Body: refusal("invalid_name", "pass one of the names toft.Mounts() lists"), Message: accessor + " needs the name an administrator gave the resource"}
	}
	available, err := Mounts()
	if err != nil {
		return Mount{}, err
	}
	var named []Mount
	for _, mount := range available {
		if mount.Name == name {
			named = append(named, mount)
		}
	}
	switch {
	case folder != nil:
		for _, mount := range named {
			if mount.Folder == *folder {
				return mount, nil
			}
		}
		if len(named) > 0 {
			hint := fmt.Sprintf("name a mounted folder: toft.%sIn(%q, %q)", accessor, name, named[0].Folder)
			return Mount{}, &Error{Body: refusal("not_mapped", hint), Message: fmt.Sprintf(
				"%q is mounted, but not at folder %q. Mounted here: %s",
				name, *folder, describeFolders(named))}
		}
	case len(named) == 1:
		return named[0], nil
	case len(named) > 1:
		hint := fmt.Sprintf("name the folder: toft.%sIn(%q, %q)", accessor, name, named[0].Folder)
		return Mount{}, &Error{Body: refusal("ambiguous_mount", hint), Message: fmt.Sprintf(
			"%d mounts are named %q — %s. Name one: toft.%sIn(%q, %q)",
			len(named), name, describeFolders(named), accessor, name, named[0].Folder)}
	}
	if len(available) == 0 {
		return Mount{}, &Error{Body: refusal("not_mapped", "ask an administrator to map storage on the app's Storage tab"), Message: fmt.Sprintf(
			"no storage resource named %q, and this app has none mapped at all — an "+
				"administrator maps storage on the app's Storage tab", name)}
	}
	// Each name once: a resource mapped at two folders is one resource.
	names := make([]string, 0, len(available))
	seen := map[string]bool{}
	for _, mount := range available {
		if !seen[mount.Name] {
			seen[mount.Name] = true
			names = append(names, mount.Name)
		}
	}
	sort.Strings(names)
	return Mount{}, &Error{Body: refusal("not_mapped", "use one of the names mapped to this app: "+strings.Join(names, ", ")), Message: fmt.Sprintf(
		"no storage resource named %q. Mapped to this app: %s", name, strings.Join(names, ", "))}
}

// describeFolders lists the folders one name is mounted at, with the level of
// each.
//
// The level is in the message because it is usually the reason there are two: a
// read corpus and a write drop are the shape this feature exists for, and a
// caller who picked the wrong one wants to see that immediately.
func describeFolders(named []Mount) string {
	parts := make([]string, 0, len(named))
	for _, mount := range named {
		access := mount.Access
		if access == "" {
			access = "read"
		}
		parts = append(parts, fmt.Sprintf("folder %q at %s", mount.Folder, access))
	}
	return strings.Join(parts, ", ")
}

// Store resolves one **store** by name.
//
// A kind-checked alias of [Storage]. Uploads are a store feature end to end —
// the CLI command is `toft store`, the grant table refuses a volume, and this is
// the accessor that matches — so a reader never has to remember which kinds are
// in scope, because the name is the kind.
//
// [Storage] keeps answering for both kinds, because app code that has a volume
// mounted has done nothing wrong.
func Store(name string) (Mount, error) {
	mount, err := storageIn(name, nil, "Store")
	return checkStore(name, mount, err)
}

// StoreIn is [Store]'s folder-naming half, on [StorageIn]'s terms.
func StoreIn(name, folder string) (Mount, error) {
	mount, err := storageIn(name, &folder, "Store")
	return checkStore(name, mount, err)
}

func checkStore(name string, mount Mount, err error) (Mount, error) {
	if err != nil {
		return Mount{}, err
	}
	if mount.Kind != "" && mount.Kind != "store" {
		hint := fmt.Sprintf("use the path toft.Storage(%q) resolves, with the ordinary os and filepath calls", name)
		return Mount{}, &Error{Body: refusal("not_a_store", hint), Message: fmt.Sprintf(
			"%q is a %s, not a store. A volume is a real filesystem — write to the path "+
				"toft.Storage(%q) resolves, with the ordinary os and filepath calls.",
			name, mount.Kind, name)}
	}
	return mount, nil
}

// refusal is the envelope a refusal this SDK makes itself carries. None of them
// is helped by trying again — a mapping, a name or a level is wrong — and with
// no body the status rule would call a statusless error retryable, which is the
// answer for a network failure. So each says so, with a code to branch on —
// and, where there is a next step, a hint naming it.
func refusal(code string, hint ...string) map[string]any {
	body := map[string]any{"code": code, "retryable": false}
	if len(hint) > 0 && hint[0] != "" {
		body["hint"] = hint[0]
	}
	return body
}

// nameHint is the next step every name refusal shares.
const nameHint = "use a relative name with forward slashes and no '.' or '..' segments"
