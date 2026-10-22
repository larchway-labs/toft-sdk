package toft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// The app's one JSON document.
//
// Settings and small state that outlive a restart, a redeploy and a rollback,
// and belong to nobody's session:
//
//	doc, err := toft.Data.Read(ctx)
//	settings := doc.Data
//	if settings == nil {
//		settings = map[string]any{"period": "monthly"}
//	}
//	settings["period"] = period
//	_, err = toft.Data.Set(ctx, settings)
//
// It sits between the two things that already existed, neither of which fits a
// handful of settings: [ScratchDir] is wiped on every start, and mapped storage
// is durable but is an administrator's separate decision per app. This needs
// nothing switched on — every app has it, because it holds only what the app put
// there and reaches nothing outside the install.
//
// **One document, not a store.** No keys, no listing, no queries, and a cap of
// 256 KiB. An app with more than that has outgrown the feature rather than hit a
// limit worth raising, and the platform's refusal says so and names what to use
// instead.
//
// **Never written and cleared are the same state, and both differ from an empty
// object.** [Document.Empty] is true for either, which is what lets an app write
// an empty document and mean it.
//
// **Two writers need [DataClient.Update].** A Set is last-write-wins, which is
// right for the single-writer app and silently loses a change when a job and the
// web process both save. Update reads, applies your change, and writes it back
// under the version it read — retrying if somebody got there first, so the other
// writer's change survives instead of being overwritten.

// DefaultUpdateAttempts is how many times [DataClient.Update] re-reads and
// re-applies before giving up.
const DefaultUpdateAttempts = 5

const dataPath = "/api/v1/app/data"

// Document is one read of the document, with the platform's two facts about it.
type Document struct {
	// Data is the document, or nil when the app has never written one.
	Data map[string]any `json:"data"`
	// Version is 0 when nothing is stored; the compare-and-set token otherwise.
	Version int `json:"version"`
	// UpdatedAt is when it was last written, zero when it never was.
	UpdatedAt time.Time `json:"updated_at"`
	// Bytes is its size, as the platform counted it against the cap.
	Bytes int `json:"bytes"`
}

// Empty reports whether nothing has been saved, or it was cleared.
func (d Document) Empty() bool { return d.Data == nil }

// UnmarshalJSON tolerates a missing or blank timestamp rather than failing the
// whole read on it. A never-written document has no updated_at, and refusing to
// decode the version alongside it would turn the ordinary first read into an
// error.
func (d *Document) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Data      map[string]any `json:"data"`
		Version   int            `json:"version"`
		UpdatedAt string         `json:"updated_at"`
		Bytes     int            `json:"bytes"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	d.Data, d.Version, d.Bytes = wire.Data, wire.Version, wire.Bytes
	d.UpdatedAt = time.Time{}
	if wire.UpdatedAt != "" {
		if when, err := time.Parse(time.RFC3339Nano, wire.UpdatedAt); err == nil {
			d.UpdatedAt = when
		}
	}
	return nil
}

// DataClient is the app's one JSON document.
type DataClient struct{}

// Get is the document, or nil when the app has never written one.
func (d DataClient) Get(ctx context.Context) (map[string]any, error) {
	doc, err := d.Read(ctx)
	if err != nil {
		return nil, err
	}
	return doc.Data, nil
}

// Read is the document *and* its version — what [DataClient.Update] is built on.
func (DataClient) Read(ctx context.Context) (Document, error) {
	t, err := client()
	if err != nil {
		return Document{}, err
	}
	raw, err := t.get(ctx, dataPath, nil)
	if err != nil {
		return Document{}, err
	}
	var doc Document
	if err := decode(raw, &doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// Set replaces the document.
//
// Version 0 and above makes the write conditional through [DataClient.SetIf]:
// plain Set is unconditional, which is right for the single-writer app.
func (d DataClient) Set(ctx context.Context, document map[string]any) (Document, error) {
	return d.write(ctx, document, nil)
}

// SetIf replaces the document only if ifVersion is still the stored version,
// and returns a [VersionConflictError] otherwise.
//
// Zero means "only if nothing is stored", which is how an app claims first write
// without a separate call.
func (d DataClient) SetIf(ctx context.Context, document map[string]any, ifVersion int) (Document, error) {
	return d.write(ctx, document, &ifVersion)
}

func (DataClient) write(ctx context.Context, document map[string]any, ifVersion *int) (Document, error) {
	// The one thing checked locally, and not a duplicated platform limit: a nil
	// map here would be serialized as null and quietly wipe the app's settings,
	// which is a wrong request rather than a refused one.
	if document == nil {
		return Document{}, &Error{Body: refusal("bad_request"), Message: "app data holds one JSON object — pass an " +
			"empty map to store an empty document, or call Clear to put the app back to " +
			"never having one"}
	}
	t, err := client()
	if err != nil {
		return Document{}, err
	}
	raw, err := t.putJSON(ctx, dataPath, document, ifMatch(ifVersion))
	if err != nil {
		return Document{}, typedConflict(err)
	}
	var doc Document
	if err := decode(raw, &doc); err != nil {
		return Document{}, err
	}
	// **The write's answer carries what was written.** The platform's reply to
	// a PUT is deliberately metadata only — version, timestamp, size — rather
	// than the document echoed back, so without this the handle came back with
	// a nil Data: the value [Document.Empty] reports as "never written", handed
	// to a caller who had just written. Filled from the document that went out
	// rather than by a second read, which would cost a round trip and could
	// answer with somebody else's later write.
	doc.Data = document
	return doc, nil
}

// Update reads, changes and writes back — without losing a concurrent writer's
// change.
//
// mutate is given the current document (an empty map when there is none) and
// returns what to store; returning the same map it was handed is the ordinary
// shape. The write is conditional on the version that was read, so if somebody
// else wrote in between, this re-reads and applies your change again rather than
// overwriting theirs:
//
//	toft.Data.Update(ctx, func(s map[string]any) (map[string]any, error) {
//		runs, _ := s["runs"].(float64)
//		s["runs"] = runs + 1
//		return s, nil
//	})
//
// Note the float64: a document that has been through the platform comes back as
// JSON, so a number read out of it is a float64 whatever an earlier write put
// in. An app that wants its own types back should unmarshal [Document.Data]
// into a struct of its own rather than reaching into the map.
//
// Use [DataClient.Read] and [DataClient.SetIf] directly when "there was nothing
// here" has to be told apart from "there was an empty document" — that
// distinction is the one thing an empty map papers over.
func (d DataClient) Update(ctx context.Context, mutate func(map[string]any) (map[string]any, error)) (Document, error) {
	return d.UpdateN(ctx, mutate, DefaultUpdateAttempts)
}

// UpdateN is [DataClient.Update] with the attempt count spelled out.
func (d DataClient) UpdateN(ctx context.Context, mutate func(map[string]any) (map[string]any, error), attempts int) (Document, error) {
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for i := 0; i < attempts; i++ {
		current, err := d.Read(ctx)
		if err != nil {
			return Document{}, err
		}
		working := current.Data
		if working == nil {
			working = map[string]any{}
		}
		changed, err := mutate(working)
		if err != nil {
			return Document{}, err
		}
		if changed == nil {
			changed = working
		}
		doc, err := d.SetIf(ctx, changed, current.Version)
		if err == nil {
			return doc, nil
		}
		var conflict *VersionConflictError
		if !errors.As(err, &conflict) {
			return Document{}, err
		}
		last = err
	}
	return Document{}, &Error{
		Message: fmt.Sprintf("app data changed under %d attempts to update it — "+
			"something is writing it continuously", attempts),
		Wrapped: last,
	}
}

// Clear throws the document away, putting the app back to never having one.
//
// Not the same as writing an empty object, deliberately: an app that cleared its
// settings and one that saved an empty document mean different things, and the
// next read tells them apart. Reports whether there was anything to remove.
func (d DataClient) Clear(ctx context.Context) (bool, error) { return d.clear(ctx, nil) }

// ClearIf is [DataClient.Clear] conditional on the stored version.
func (d DataClient) ClearIf(ctx context.Context, ifVersion int) (bool, error) {
	return d.clear(ctx, &ifVersion)
}

func (DataClient) clear(ctx context.Context, ifVersion *int) (bool, error) {
	t, err := client()
	if err != nil {
		return false, err
	}
	raw, err := t.delete(ctx, dataPath, ifMatch(ifVersion))
	if err != nil {
		return false, typedConflict(err)
	}
	var body struct {
		Deleted bool `json:"deleted"`
	}
	if err := decode(raw, &body); err != nil {
		return false, err
	}
	return body.Deleted, nil
}

func ifMatch(version *int) map[string]string {
	if version == nil {
		return nil
	}
	return map[string]string{"If-Match": fmt.Sprintf("%q", strconv.Itoa(*version))}
}

// typedConflict gives a failed precondition a type, so [DataClient.Update] can
// retry it and app code can catch it with errors.As, rather than matching on a
// status code that other refusals also use.
func typedConflict(err error) error {
	platform, ok := AsError(err)
	if !ok || platform.Code() != "version_conflict" {
		return err
	}
	version := 0
	if raw, ok := platform.Body["version"].(float64); ok {
		version = int(raw)
	}
	return &VersionConflictError{Err: platform, Version: version}
}
