package toft

import (
	"bytes"
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
//
// Encoded, it carries the documented fields of a mounts() entry in every SDK —
// name, kind, path, access, folder, read_only and mounted — the last two
// derived, as [Mount.ReadOnly] and [Mount.Mounted] derive them.
type Mount struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Access string `json:"access"`
	// Folder is which directory of the resource this mount is rooted at, and
	// "" is the whole resource. It is here because **two mounts of one resource
	// share a name**, and the name is how app code locates a resource — so this
	// is what tells them apart. Deliberately not part of Path: where a mount
	// appears stays the administrator's choice.
	Folder string `json:"folder"`
}

// MarshalJSON adds read_only and mounted to the stored fields, so an encoded
// Mount reads as the mounts() entry of the other three SDKs.
func (m Mount) MarshalJSON() ([]byte, error) {
	type plain Mount
	return json.Marshal(struct {
		plain
		ReadOnly bool `json:"read_only"`
		Mounted  bool `json:"mounted"`
	}{plain(m), m.ReadOnly(), m.Mounted()})
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
	if err := m.checkMapped(); err != nil {
		return "", err
	}
	// Put is a copy onto the mount, so a mounted volume takes it as a mounted
	// store does — as Python's put(), Node's put() and R's store_put() do. The
	// signed-URL methods (Upload, List, URL, Download, Remove) stay a store's
	// alone, since a volume has no data plane (reverses BB-055's refusal,
	// 2026-11-04: the four SDKs agree on the permissive answer).
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
	in, err := os.Open(source)
	if err != nil {
		return "", localFileError("read", source, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", localWriteError(destination, err)
	}
	out, err := os.Create(destination)
	if err != nil {
		return "", localWriteError(destination, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return "", localWriteError(destination, err)
	}
	if err := out.Close(); err != nil {
		return "", localWriteError(destination, err)
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
		// Empty, not nil: encoded, it is [] — the list every other SDK answers
		// — rather than null.
		return []Mount{}, nil
	}
	type storageEntry struct {
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Path   string `json:"path"`
		Access string `json:"access"`
		Folder string `json:"folder"`
	}
	// Decoded in two steps so a refusal names what is wrong in the variable's
	// own terms — "an entry is not an object" — rather than quoting
	// encoding/json's Go types at somebody who wrote JSON (BB-096).
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, &Error{Body: refusal("storage_env_invalid", storageEnvHint), Message: fmt.Sprintf("%s is not a JSON array of resources", EnvStorage), Wrapped: err}
	}
	if items == nil {
		// "null" decodes into a nil slice without complaint; it is not a list.
		return nil, &Error{Body: refusal("storage_env_invalid", storageEnvHint), Message: fmt.Sprintf("%s is not a JSON array of resources: null", EnvStorage)}
	}
	out := make([]Mount, 0, len(items))
	for _, item := range items {
		item = bytes.TrimSpace(item)
		if len(item) == 0 || item[0] != '{' {
			return nil, &Error{Body: refusal("storage_env_invalid", storageEnvHint),
				Message: fmt.Sprintf("a %s entry is not an object: %s", EnvStorage, item)}
		}
		var entry storageEntry
		if err := json.Unmarshal(item, &entry); err != nil {
			return nil, &Error{Body: refusal("storage_env_invalid", storageEnvHint),
				Message: fmt.Sprintf("a %s entry's name, kind, path, access and folder are strings: %s", EnvStorage, item), Wrapped: err}
		}
		// The platform writes every entry with a name, a kind and an access
		// level (and a path, "" for a store reached over signed URLs). An entry
		// missing one is a hand-written stand-in that would resolve to a mount
		// with no kind — which Store takes for a store and Storage for a volume —
		// so it is refused rather than read as empty strings.
		for _, field := range [][2]string{{"name", entry.Name}, {"kind", entry.Kind}, {"access", entry.Access}} {
			if key, value := field[0], field[1]; strings.TrimSpace(value) == "" {
				encoded, _ := json.Marshal(entry)
				return nil, &Error{Body: refusal("storage_env_invalid", storageEnvHint),
					Message: fmt.Sprintf("a %s entry has no %s: %s", EnvStorage, key, encoded)}
			}
		}
		// Held to the values the platform writes: a kind it does not know is a
		// mount Store and Storage would each guess at, and an access level it
		// does not know makes ReadOnly answer false for a mount nothing granted
		// write on — as Python, Node and R refuse them.
		for _, field := range []struct {
			key, value string
			allowed    []string
		}{{"kind", entry.Kind, storageKinds}, {"access", entry.Access, storageAccess}} {
			if !contains(field.allowed, field.value) {
				encoded, _ := json.Marshal(entry)
				return nil, &Error{Body: refusal("storage_env_invalid", storageEnvHint),
					Message: fmt.Sprintf("a %s entry's %s is %q, not one of %s: %s", EnvStorage, field.key,
						field.value, strings.Join(field.allowed, ", "), encoded)}
			}
		}
		out = append(out, Mount(entry))
	}
	return out, nil
}

var (
	storageKinds  = []string{"volume", "store"}
	storageAccess = []string{"read", "write", "full"}
)

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

const storageEnvHint = "TOFT_STORAGE is a JSON array of objects, each with name, kind (volume or store) and access (read, write or full), " +
	"plus path where the resource is mounted (a store reached by signed URL has none); " +
	"the platform writes it, and a local stand-in needs the same keys"

// checkFolder holds a folder to the rule an object's name is held to —
// relative, forward slashes, no "." or ".." segments — with "" naming the root.
// Without it StorageIn("x", "..") was not_mapped, "mounted, but not at folder",
// a lookup failing where the argument was the fault.
func checkFolder(folder, accessor string) error {
	if folder == "" {
		return nil
	}
	bad := strings.HasPrefix(folder, "/") || strings.HasSuffix(folder, "/") || strings.Contains(folder, `\`)
	for _, segment := range strings.Split(folder, "/") {
		if segment == "" || segment == "." || segment == ".." {
			bad = true
		}
	}
	if !bad {
		return nil
	}
	return &Error{Body: refusal("invalid_name", fmt.Sprintf(`name a folder of the resource, like toft.%sIn("drop", "backup"), or "" for its root`, accessor)),
		Message: fmt.Sprintf("a folder is a relative path inside the resource, with forward slashes and no '.' or '..' segments, got %q", folder)}
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
	if folder != nil {
		if err := checkFolder(*folder, accessor); err != nil {
			return Mount{}, err
		}
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
	if len(available) == 0 && !onPlatform() {
		// Off the platform there is nothing to map, and "ask an administrator"
		// would send somebody to one over a laptop — the code Node raises.
		return Mount{}, &Error{Body: refusal("off_platform", "set "+EnvStorage+" to a JSON array of mounts to stand one in locally"),
			Message: fmt.Sprintf("%s(%q) needs the storage Toft maps into a deployed app (%s) — it only resolves in an app running on the platform", accessor, name, EnvStorage)}
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
		resolve := volumeAccessor(name, mount)
		hint := fmt.Sprintf("use the path %s resolves, with the ordinary os and filepath calls", resolve)
		return mount, &Error{Body: refusal("not_a_store", hint), Message: fmt.Sprintf(
			"%q is a %s, not a store. A volume is a real filesystem — write to the path "+
				"%s resolves, with the ordinary os and filepath calls.",
			name, mount.Kind, resolve)}
	}
	return mount, nil
}

// volumeAccessor is the call that resolves a volume's path: StorageIn when the
// mount is a folder, because Storage on a name mounted at several folders is
// ambiguous_mount (BB-083).
func volumeAccessor(name string, m Mount) string {
	if m.Folder != "" {
		return fmt.Sprintf("toft.StorageIn(%q, %q)", name, m.Folder)
	}
	return fmt.Sprintf("toft.Storage(%q)", name)
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

// onPlatform reports whether any variable the platform sets on every workload
// is here — the test Node's SDK makes for the same refusal.
func onPlatform() bool {
	for _, name := range []string{"TOFT_API_URL", "TOFT_APP_ID", "TOFT_RUN_ID", "TOFT_SCRATCH_DIR"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

// nameHint is the next step every name refusal shares.
const nameHint = "use a relative name with forward slashes and no '.' or '..' segments"
