package toft

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // the object store's integrity check, not a security boundary
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// A store over the platform's signed URLs.
//
// List, Upload, Download, URL and Remove reach a store through
// /api/v1/app/store, in both execution modes — so an app written against them
// moves between modes unchanged. On a shared host a store is never mounted at
// all: its entry carries no Path, and these are the whole of how the app
// reaches it.
//
//	uploads, _ := toft.Store("uploads")
//	uploads.Upload(ctx, "report.csv", "")
//	link, _ := uploads.URL(ctx, "report.csv", true) // hand to a browser
//
// The platform signs and never sees the bytes; the bytes go straight to the
// object store, carrying the signed headers verbatim and no Toft token.

// Listing is one page of a store's listing. Names are relative to the path
// listed; NextCursor is empty on the last page.
type Listing struct {
	Objects []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		ETag string `json:"etag,omitempty"`
	} `json:"objects"`
	Prefixes   []string `json:"prefixes"`
	NextCursor string   `json:"next_cursor"`
	Access     string   `json:"access"`
}

type signedRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
}

// List reads one page of what is under path ("" for the top).
func (m Mount) List(ctx context.Context, path, cursor string) (Listing, error) {
	if err := m.checkMapped(); err != nil {
		return Listing{}, err
	}
	if path != "" {
		if err := checkRelativeName(strings.TrimSuffix(path, "/")); err != nil {
			return Listing{}, err
		}
	}
	route, err := m.storeRoute("/api/v1/app/store/{slug}/objects", "", map[string]string{"path": path, "cursor": cursor})
	if err != nil {
		return Listing{}, err
	}
	t, err := client()
	if err != nil {
		return Listing{}, err
	}
	raw, err := t.get(ctx, route, nil)
	if err != nil {
		return Listing{}, storeRefusal(err)
	}
	var page Listing
	return page, decode(raw, &page)
}

// Upload sends a local file as name (its own name when name is ""), and
// returns the name it landed under. At write this creates new objects only,
// and an existing name is a refusal that says so.
func (m Mount) Upload(ctx context.Context, source, name string) (string, error) {
	if err := m.checkMapped(); err != nil {
		return "", err
	}
	target := name
	if target == "" {
		target = filepath.Base(source)
	}
	if err := checkRelativeName(target); err != nil {
		return "", err
	}
	// Streamed, never held: the file is read once to hash it and once to send
	// it, so an upload costs a buffer rather than its size — an app under a
	// memory limit could not otherwise upload anything larger than that limit.
	f, err := os.Open(source)
	if err != nil {
		return "", localFileError("read", source, err)
	}
	defer f.Close()
	sha, sum := sha256.New(), md5.New() //nolint:gosec
	size, err := io.Copy(io.MultiWriter(sha, sum), f)
	if err != nil {
		return "", localFileError("read", source, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", localFileError("read", source, err)
	}
	route, err := m.storeRoute("/api/v1/app/store/{slug}/objects", "", nil)
	if err != nil {
		return "", err
	}
	var signed signedRequest
	if err := m.mint(ctx, http.MethodPost, route, map[string]any{
		"name": target, "size": size,
		"sha256": hex.EncodeToString(sha.Sum(nil)), "md5": hex.EncodeToString(sum.Sum(nil)),
	}, &signed); err != nil {
		return "", err
	}
	if _, err := sendSignedReader(ctx, signed, io.LimitReader(f, size), size); err != nil {
		var e *Error
		if errors.As(err, &e) && e.Status == http.StatusPreconditionFailed {
			// `write`, not m.Access: the store signs at the mapping's level
			// *now*, and only `write` is refused an existing name — the cached
			// level was the one the app started with.
			return "", &Error{Body: refusal("already_exists", "upload under a new name, or ask an administrator for `full` access where the store is mapped"), Status: e.Status, Message: fmt.Sprintf(
				"%q already exists in %q, and this app's access there is `write`, which creates new objects only",
				target, m.Name)}
		}
		return "", err
	}
	return target, nil
}

// Download fetches one object's bytes.
func (m Mount) Download(ctx context.Context, name string) ([]byte, error) {
	signed, err := m.signGet(ctx, name, false)
	if err != nil {
		return nil, err
	}
	data, err := sendSigned(ctx, signed, nil)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		// The URL is signed for any name; the store is what answers that
		// nothing is there — the first night of every nightly job.
		return nil, &Error{Body: refusal("not_found", "call List to see the names that are there"), Status: e.Status, Message: fmt.Sprintf(
			"%q is not in %q — nothing has been put there under that name yet", name, m.Name)}
	}
	return data, err
}

// URL is a signed GET for one object, to hand to a browser. It lives fifteen
// minutes and anyone holding it reads that object until then — the point for a
// download link, and the reason not to log one. attachment makes a browser save
// it under the object's own name.
func (m Mount) URL(ctx context.Context, name string, attachment bool) (string, error) {
	signed, err := m.signGet(ctx, name, attachment)
	return signed.URL, err
}

// Remove deletes one object. It needs full.
func (m Mount) Remove(ctx context.Context, name string) error {
	if err := m.checkMapped(); err != nil {
		return err
	}
	if err := checkRelativeName(name); err != nil {
		return err
	}
	route, err := m.storeRoute("/api/v1/app/store/{slug}/objects/{name}", name, nil)
	if err != nil {
		return err
	}
	var signed signedRequest
	if err := m.mint(ctx, http.MethodDelete, route, nil, &signed); err != nil {
		return err
	}
	_, err = sendSigned(ctx, signed, nil)
	return err
}

func (m Mount) signGet(ctx context.Context, name string, attachment bool) (signedRequest, error) {
	if err := m.checkMapped(); err != nil {
		return signedRequest{}, err
	}
	if err := checkRelativeName(name); err != nil {
		return signedRequest{}, err
	}
	query := map[string]string{}
	if attachment {
		query["disposition"] = "attachment"
	}
	route, err := m.storeRoute("/api/v1/app/store/{slug}/objects/{name}", name, query)
	if err != nil {
		return signedRequest{}, err
	}
	var signed signedRequest
	return signed, m.mint(ctx, http.MethodGet, route, nil, &signed)
}

func (m Mount) mint(ctx context.Context, method, route string, body any, into *signedRequest) error {
	t, err := client()
	if err != nil {
		return err
	}
	var raw []byte
	switch method {
	case http.MethodPost:
		raw, err = t.postJSON(ctx, route, body, "", nil)
	case http.MethodDelete:
		raw, err = t.delete(ctx, route, nil)
	default:
		raw, err = t.get(ctx, route, nil)
	}
	if err != nil {
		return storeRefusal(err)
	}
	if err := decode(raw, into); err != nil {
		return err
	}
	if into.URL == "" {
		return &Error{Message: "the platform returned no signed URL"}
	}
	return nil
}

// storeRoute fills in one of the platform's routes, spelled whole at each call
// so a reader — and the SDK contract test — sees which one it reaches. The
// folder always travels, "" included: it names *this* mapping, so a resource
// mapped twice is never a question the platform has to ask back.
func (m Mount) storeRoute(route, name string, query map[string]string) (string, error) {
	if err := m.checkMapped(); err != nil {
		return "", err
	}
	if m.Kind != "" && m.Kind != "store" {
		hint := fmt.Sprintf("use the path toft.Storage(%q) resolves, with the ordinary os and filepath calls", m.Name)
		return "", &Error{Body: refusal("not_a_store", hint), Message: fmt.Sprintf(
			"%q is a %s, which is reached at its path — only an object store is reached "+
				"over signed URLs", m.Name, m.Kind)}
	}
	params := url.Values{"folder": {m.Folder}}
	for k, v := range query {
		if v != "" {
			params.Set(k, v)
		}
	}
	route = strings.Replace(route, "{slug}", url.PathEscape(m.Name), 1)
	route = strings.Replace(route, "{name}", escapeName(name), 1)
	return route + "?" + params.Encode(), nil
}

// checkMapped refuses a Mount that names no resource — the zero Mount that
// [Store] returns beside its not_mapped error — with that same not_mapped, so
// every operation on an unmapped store answers what Store("nope") answered,
// rather than a request for an empty slug the platform calls route_not_found.
func (m Mount) checkMapped() error {
	if m.Name != "" {
		return nil
	}
	return &Error{Body: refusal("not_mapped", "resolve the store with toft.Store(name) and check its error before using it"),
		Message: "this Mount names no storage resource — it is the zero value toft.Store returns when " +
			"the name is not mapped to this app"}
}

// storeRefusal gives the platform's refusal for a store it does not map to this
// app the SDK's own code, not_mapped — the code toft.Store("nope") raises
// locally. The platform says store_not_mapped for the same fact (a Mount built
// by hand, or one that outlived its mapping), and one fact answering under two
// codes is two branches every app has to know to write.
func storeRefusal(err error) error {
	e, ok := AsError(err)
	if !ok || e.Code() != "store_not_mapped" {
		return err
	}
	out := *e
	out.Body = make(map[string]any, len(e.Body)+1)
	for k, v := range e.Body {
		out.Body[k] = v
	}
	out.Body["code"] = "not_mapped"
	out.Body["platform_code"] = "store_not_mapped"
	return &out
}

// escapeName escapes each segment of an object name and keeps its slashes.
func escapeName(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// sendSigned carries out a signed request against the object store: the signed
// headers verbatim, and no Toft header at all.
func sendSigned(ctx context.Context, signed signedRequest, body []byte) ([]byte, error) {
	if body == nil {
		return sendSignedReader(ctx, signed, nil, 0)
	}
	return sendSignedReader(ctx, signed, bytes.NewReader(body), int64(len(body)))
}

// sendSignedReader is sendSigned with the body streamed: size is sent as the
// Content-Length, which a signed PUT needs and a reader cannot announce.
func sendSignedReader(ctx context.Context, signed signedRequest, body io.Reader, size int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	method := signed.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, signed.URL, body)
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("the signed request: %v", err), Wrapped: err}
	}
	if body != nil {
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
	}
	for k, v := range signed.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("cannot reach the object store: %v", err), Wrapped: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, objectStoreRefusal("store_refused", fmt.Sprintf("the object store refused the request: %s", resp.Status), resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
