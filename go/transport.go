package toft

// The HTTP transport for /api/v1/app/*.
//
// Standard library only, on purpose. This compiles into somebody's app beside
// their own dependency tree, and a version conflict between the SDK's HTTP
// library and theirs would be our bug appearing in their build. Go makes that
// promise cheap to keep: net/http and encoding/json are enough.
//
// Two things here are worth reading before changing them.
//
// **The retry is exactly one, and it is not a retry policy.** The platform's
// own queue does the real work (one retry against the transport, then the send
// is recorded failed). This retry covers the far narrower case of the request
// never reaching a handler: a 429 from the rate limiter, or a 5xx from in front
// of it. Anything more here would be a second source of truth about how a send
// behaves, and the record in the platform — not this client — is what a
// workflow relies on.
//
// **A retried request reuses its idempotency key**, which is the whole reason
// the key is chosen by the caller rather than generated per attempt. Without
// that, this retry is precisely the thing that sends the weekly report twice.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout = 30 * time.Second

	// How long to wait before the single retry when the platform did not say.
	defaultRetryAfter = 2 * time.Second

	// An upload can be much slower than a JSON call, and timing one out at 30s
	// turns a working 20 MiB report into a mystery.
	uploadTimeout = 5 * time.Minute
)

// Attachment is one file in a multipart request — an email attachment, or the
// file behind [NotifyClient.SlackFile].
type Attachment struct {
	// Name is what the recipient sees. Reduced to a basename before it goes on
	// the wire, here as well as on the platform: not because this side is
	// trusted — it is not, and the platform sanitizes again — but because a
	// caller passing "/tmp/x/report.xlsx" means the file, and silently mailing
	// a filename with somebody's directory layout in it is a small disclosure
	// nobody chose.
	Name string

	Content []byte

	// Type is the MIME type. Guessed from the name when empty.
	Type string
}

// AttachFile reads a local file into an [Attachment].
func AttachFile(path string) (Attachment, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Attachment{}, &Error{Message: fmt.Sprintf("cannot read %s: %v", path, err), Wrapped: err}
	}
	return Attachment{Name: filepath.Base(path), Content: body}, nil
}

func (a Attachment) normalized() Attachment {
	out := a
	out.Name = filepath.Base(strings.ReplaceAll(a.Name, `\`, "/"))
	if out.Name == "" || out.Name == "." || out.Name == "/" {
		out.Name = "attachment"
	}
	if out.Type == "" {
		out.Type = mime.TypeByExtension(filepath.Ext(out.Name))
	}
	if out.Type == "" {
		out.Type = "application/octet-stream"
	}
	return out
}

type transport struct {
	base   string
	token  string
	client *http.Client
}

// newTransport builds the transport from the environment, or reports the
// variable that is missing.
func newTransport() (*transport, error) {
	base, err := require(EnvAPIURL)
	if err != nil {
		return nil, err
	}
	token, err := appOrJobToken()
	if err != nil {
		return nil, err
	}
	return &transport{base: trimSlash(base), token: token, client: noRedirectClient()}, nil
}

// appOrJobToken is the app's own token in its service process; inside a job,
// which holds no app token, the run's own credential — accepted by the platform
// for sending through the app's integrations and nothing else (ADR §108). So
// Notify works from a job, and Data from a job is refused with job_send_only.
func appOrJobToken() (string, error) {
	if t := strings.TrimSpace(os.Getenv(EnvAppToken)); t != "" {
		return t, nil
	}
	if strings.TrimSpace(os.Getenv(EnvRunID)) != "" {
		if t := strings.TrimSpace(os.Getenv("TOFT_JOB_TOKEN")); t != "" {
			return t, nil
		}
	}
	return require(EnvAppToken)
}

func trimSlash(base string) string { return strings.TrimRight(base, "/") }

// noRedirectClient never follows a redirect on its own: see follow for why the
// one route that redirects is followed by hand instead.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func (t *transport) get(ctx context.Context, path string, headers map[string]string) ([]byte, error) {
	return t.attempt(ctx, http.MethodGet, path, nil, "", headers, defaultTimeout)
}

func (t *transport) postJSON(ctx context.Context, path string, body any, idempotencyKey string, headers map[string]string) ([]byte, error) {
	encoded, err := encode(body)
	if err != nil {
		return nil, err
	}
	return t.attempt(ctx, http.MethodPost, path, encoded, "application/json",
		withIdempotency(headers, idempotencyKey), defaultTimeout)
}

// putJSON replaces one resource, rather than enqueueing one thing.
//
// No idempotency key, deliberately: a PUT replaces the same resource whatever
// the attempt number, so the key that keeps a retried POST from starting two
// runs has nothing to protect here. If-Match is the header that matters
// instead, and it belongs to the caller.
func (t *transport) putJSON(ctx context.Context, path string, body any, headers map[string]string) ([]byte, error) {
	encoded, err := encode(body)
	if err != nil {
		return nil, err
	}
	return t.attempt(ctx, http.MethodPut, path, encoded, "application/json", headers, defaultTimeout)
}

func (t *transport) delete(ctx context.Context, path string, headers map[string]string) ([]byte, error) {
	return t.attempt(ctx, http.MethodDelete, path, nil, "", headers, defaultTimeout)
}

func (t *transport) postMultipart(ctx context.Context, path string, fields map[string]string, field string, parts []Attachment, idempotencyKey string) ([]byte, error) {
	body, contentType, err := encodeMultipart(fields, field, parts)
	if err != nil {
		return nil, err
	}
	return t.attempt(ctx, http.MethodPost, path, body, contentType,
		withIdempotency(nil, idempotencyKey), uploadTimeout)
}

// follow GETs a route that redirects to a store, and returns the bytes.
//
// The redirect is followed by hand, deliberately. A client that carried this
// app's platform token onto a redirected request would hand it to whatever host
// the object store lives on — a credential leak in exchange for three lines
// saved. The second request carries no Toft headers at all, because a
// presigned URL is already the whole authorization.
//
// Returns nil when the platform says there is nothing to fetch; a refusal it
// can explain (an expired artifact, a capability that is off) comes back with
// the platform's own words.
func (t *transport) follow(ctx context.Context, path string, headers map[string]string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	req, err := t.request(ctx, http.MethodGet, path, nil, "", headers)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, t.unreachable(err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// fall through to the object store
	case resp.StatusCode >= 400:
		return nil, fromResponse(resp)
	default:
		return io.ReadAll(resp.Body)
	}

	location := resp.Header.Get("Location")
	if location == "" {
		return nil, &Error{Message: "the platform redirected without a location", Status: resp.StatusCode}
	}
	return fetchPresigned(ctx, location)
}

// fetchPresigned reads an object over a URL the platform minted. No Toft
// headers: the signature is the whole authorization, and adding the app's token
// would send it to a host that has no business holding it.
func fetchPresigned(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("fetching the object: %v", err), Wrapped: err}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("fetching the object: %v", err), Wrapped: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, &Error{
			Message: fmt.Sprintf("fetching the object: %s", resp.Status),
			Status:  resp.StatusCode,
		}
	}
	return io.ReadAll(resp.Body)
}

func (t *transport) attempt(ctx context.Context, method, path string, body []byte, contentType string, headers map[string]string, timeout time.Duration) ([]byte, error) {
	out, err := t.once(ctx, method, path, body, contentType, headers, timeout)
	if err == nil {
		return out, nil
	}
	platform, ok := AsError(err)
	if !ok || !platform.autoRetry() {
		return nil, err
	}
	wait := platform.RetryAfter
	if !platform.retryGiven {
		wait = defaultRetryAfter
	}
	select {
	case <-ctx.Done():
		return nil, &Error{Message: ctx.Err().Error(), Wrapped: ctx.Err()}
	case <-time.After(wait):
	}
	return t.once(ctx, method, path, body, contentType, headers, timeout)
}

func (t *transport) once(ctx context.Context, method, path string, body []byte, contentType string, headers map[string]string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := t.request(ctx, method, path, body, contentType, headers)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, t.unreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fromResponse(resp)
	}
	// A redirect on an ordinary call is the platform answering a question this
	// method did not ask; only follow deals with those.
	if resp.StatusCode >= 300 {
		return nil, &Error{
			Message: fmt.Sprintf("unexpected redirect from %s", path),
			Status:  resp.StatusCode,
		}
	}
	return io.ReadAll(resp.Body)
}

func (t *transport) request(ctx context.Context, method, path string, body []byte, contentType string, headers map[string]string) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, reader)
	if err != nil {
		return nil, &Error{Message: err.Error(), Wrapped: err}
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set(ContractHeader, AppContractVersion)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return req, nil
}

func (t *transport) unreachable(err error) error {
	return &Error{
		Message: fmt.Sprintf("cannot reach Toft at %s: %v", t.base, err),
		Wrapped: err,
	}
}

// fromResponse turns the platform's error body into the message a developer
// reads.
//
// Refusals from /api/v1/app/* are written for a person and name the remedy —
// "email is not enabled for this app; an administrator can enable it on the
// app's Integrations tab". Flattening that into "403 Forbidden" throws away the
// only part that helps.
func fromResponse(resp *http.Response) *Error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := &Error{Status: resp.StatusCode, Body: map[string]any{}}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err == nil && parsed != nil {
		out.Body = parsed
		if detail, ok := parsed["error"].(string); ok {
			out.Message = detail
		}
	}
	if out.Message == "" {
		out.Message = strings.TrimSpace(string(raw))
	}
	if out.Message == "" {
		out.Message = resp.Status
	}
	if header := resp.Header.Get("Retry-After"); header != "" {
		if seconds, err := strconv.ParseFloat(header, 64); err == nil && seconds >= 0 {
			out.RetryAfter, out.retryGiven = time.Duration(seconds*float64(time.Second)), true
		}
	}
	return out
}

func encode(body any) ([]byte, error) {
	if body == nil {
		return []byte("{}"), nil
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("cannot encode the request body: %v", err), Wrapped: err}
	}
	return out, nil
}

// encodeMultipart builds a multipart/form-data body. Every part goes under the
// one field name, because that is what the two routes that take files want:
// "file" for a Slack upload, "attachment" for mail.
func encodeMultipart(fields map[string]string, field string, parts []Attachment) ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			return nil, "", &Error{Message: err.Error(), Wrapped: err}
		}
	}
	for _, raw := range parts {
		part := raw.normalized()
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", fmt.Sprintf(
			`form-data; name=%q; filename=%q`, field, quoteName(part.Name)))
		header.Set("Content-Type", part.Type)
		w, err := writer.CreatePart(header)
		if err != nil {
			return nil, "", &Error{Message: err.Error(), Wrapped: err}
		}
		if _, err := w.Write(part.Content); err != nil {
			return nil, "", &Error{Message: err.Error(), Wrapped: err}
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", &Error{Message: err.Error(), Wrapped: err}
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

func quoteName(name string) string {
	return strings.NewReplacer(`\`, "", `"`, "", "\r", "", "\n", "").Replace(name)
}

func withIdempotency(headers map[string]string, key string) map[string]string {
	if key == "" {
		return headers
	}
	merged := make(map[string]string, len(headers)+1)
	for name, value := range headers {
		merged[name] = value
	}
	merged["Idempotency-Key"] = key
	return merged
}

// freshKey mints an idempotency key for one call.
//
// Generated once per call rather than per HTTP attempt, which is the entire
// point: the transport's single retry presents the same key, so a request that
// was in fact delivered before the response was lost does not send twice.
func freshKey() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// Only reachable if the kernel's entropy source is broken, and a
		// duplicate report is a better outcome than a failed send.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(raw[:])
}

func decode(raw []byte, into any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return &Error{Message: fmt.Sprintf("the platform sent a body this SDK could not read: %v", err), Wrapped: err}
	}
	return nil
}
