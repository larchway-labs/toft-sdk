package toft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// A running job's view of itself, over the runner callback contract.
//
// This is the *inside* of a job — code executing as the workload, reporting back
// over /api/v1/runner/* with the run-scoped token the runner exported. It is the
// other side of [JobsClient], which is how an app *starts* one:
//
//	run := toft.CurrentRun() // nil outside a job
//	var params struct{ Month string }
//	run.Params(&params)
//	rows := reconcile(params.Month)
//	run.Progress(ctx, 80)
//	run.WriteOutput(ctx, csvBytes)   // presigned PUT, both modes
//	run.SetResult(ctx, map[string]any{"rows": len(rows), "output": "reconcile/2026-03.csv"})
//
// SetResult and WriteOutput are the two durable channels, in the order to reach
// for them. The structured result is right for what most internal tools produce —
// counts, a summary, a few hundred rows — costs no configuration, and comes back
// on every read of the run. The output object is for the artifact itself.
//
// **Never write a durable result into scratch.** TOFT_SCRATCH_DIR is wiped
// on every start and [SessionScratchDir] is per-login, so a report written there
// is a dead path tomorrow while the run row cheerfully still says succeeded.
// Status outliving its payload is the worst failure shape available here, which
// is why a result should carry a storage-relative reference rather than a raw
// path.

// runnerContractVersion is the callback contract this SDK speaks, and it must
// equal the platform's runner.ContractVersion. A partial upgrade is reported
// rather than tolerated, so a stale number here is a 409 on every callback a job
// makes. sdk/contract_test.go keeps the four copies of it in step.
const runnerContractVersion = "2"

// Run is the job's own view of itself: report progress, read input, write output.
//
// [CurrentRun] configures it from what the runner already exported, so a job
// never handles a credential explicitly.
type Run struct {
	// ID is the run this process is executing.
	ID string

	// Trigger is *how* this run started: "cron", "manual", "api" or "app".
	// TriggerRole is *who* — one of [RoleOwner], [RoleEditor], [RoleViewer],
	// [RoleAnonymous], [RoleSystem] (a schedule firing on its schedule) or
	// [RoleApp] (the app starting work for itself), and [RoleUnknown] where the
	// platform did not say.
	//
	// The pair is the point. "manual" tells a report job nothing about whether
	// to include the private columns; "manual" plus "viewer" does. Both have
	// been recorded on every run since jobs existed and neither had a name in
	// this SDK until now.
	//
	// **The role is today's, not the day the run was queued.** It is derived
	// from the run's actor when the workload starts, so somebody demoted in
	// between reports their current role — which is the right answer for a job
	// deciding what work to do, and the wrong one for an audit. The audit
	// question is answered by the run's actor in Toft's own history, which
	// does not move.
	Trigger     string
	TriggerRole string

	// InputURI and OutputURI are presigned locations the platform minted. A job
	// holds no object-store credential, because one that could read this run's
	// input could read every other app's.
	InputURI  string
	OutputURI string

	base   string
	token  string
	client *http.Client
}

// CurrentRun is the job's own view of itself, or nil outside a job.
//
// nil rather than an error, because `if run := toft.CurrentRun(); run != nil` is how
// a package shared between an app's web entrypoint and its job entrypoint tells
// which one it is in — and that is exactly the shape this feature encourages, one
// deployable with two entrypoints.
func CurrentRun() *Run {
	base := os.Getenv(EnvAPIURL)
	token := os.Getenv("TOFT_JOB_TOKEN")
	id := os.Getenv("TOFT_RUN_ID")
	if base == "" || token == "" || id == "" {
		return nil
	}
	// Absent means the platform did not say, and unknown is the safe reading of
	// that everywhere else in this SDK too.
	triggerRole := os.Getenv("TOFT_TRIGGER_ROLE")
	if triggerRole == "" {
		triggerRole = RoleUnknown
	}
	return &Run{
		ID:          id,
		Trigger:     os.Getenv("TOFT_TRIGGER"),
		TriggerRole: triggerRole,
		InputURI:    os.Getenv("TOFT_INPUT_URI"),
		OutputURI:   os.Getenv("TOFT_OUTPUT_URI"),
		base:        trimSlash(base),
		token:       token,
		// Deliberately not the app transport. That one carries the app token to
		// /api/v1/app/*; this carries a credential minted for one run, valid only
		// until that run's deadline, and accepted only on the callback routes.
		// The audiences are what keep a job's progress bar from being able to act
		// as the app — sharing one client between them would be the first step
		// towards sharing a credential.
		client: &http.Client{Timeout: defaultTimeout},
	}
}

// Params unmarshals the parameters this run was started with into value.
//
// A struct of the app's own is the point: the parameters were recorded for audit
// as JSON, and reading them back through a typed shape is where a job notices
// that a caller sent a month it cannot parse.
func (r *Run) Params(value any) error {
	raw := os.Getenv("TOFT_PARAMS")
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), value); err != nil {
		return &Error{Message: fmt.Sprintf("this run's parameters are not the shape this job expects: %v", err), Wrapped: err}
	}
	return nil
}

// ScheduledFor is the instant this run is for, and false when nobody gave one.
//
// A scheduled run's is the tick it fired for — the 03:00 run is the 03:00 run
// even if it started at 03:40 — in the schedule's own zone, which the returned
// time carries as its offset. A run started with `toft job run --for` carries what
// was named. Never defaulted to now: a re-run of Tuesday's work on Thursday must
// not quietly process Thursday.
func (r *Run) ScheduledFor() (time.Time, bool) {
	raw := os.Getenv(EnvScheduledFor)
	if raw == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// Progress reports how far along this run is, 0–100.
func (r *Run) Progress(ctx context.Context, percent int) error {
	_, err := r.call(ctx, "/api/v1/runner/progress", map[string]any{
		"version": runnerContractVersion, "percent": percent,
	})
	return err
}

// SetResult records the run's structured result.
func (r *Run) SetResult(ctx context.Context, result any) error {
	_, err := r.call(ctx, "/api/v1/runner/result", map[string]any{
		"version": runnerContractVersion, "result": result, "exit_code": 0,
		// The run is still going; only the runner's own report at exit ends it.
		"record_only": true,
	})
	return err
}

// ReadInput fetches the payload this run was given, or nil when it was given
// none.
//
// **A refusal here is named rather than relayed.** The platform mints this URL
// only for a run that was actually given a payload, so a 404 means an install
// that predates that rule; a 403 or 410 is the link's six-hour window having
// closed under a very long run. Both used to surface as "fetching the object:
// 404 Not Found" — a failure an author could not tell from a store that was
// down.
func (r *Run) ReadInput(ctx context.Context) ([]byte, error) {
	if r.InputURI == "" {
		return nil, nil
	}
	raw, err := fetchPresigned(ctx, r.InputURI)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Status != 0 {
			return nil, &Error{Body: inputRefusalBody(e.Status), Message: inputRefusal(e.Status),
				Status: e.Status, Wrapped: err}
		}
		return nil, err
	}
	return raw, nil
}

// inputRefusal is what a failed input read means, in the two cases that are not
// "it broke".
//
// Written out because the failures are indistinguishable at the HTTP layer and
// the fixes are opposite: one is a job asking for something it was never given,
// the other is a job that took too long. The same two sentences the Python and
// Node SDKs raise.
// inputRefusalBody names the two refusals [inputRefusal] explains, so an agent
// branches on input_expired rather than on the sentence. Any other status is
// left to the status rule: a 503 from the object store is worth another try.
func inputRefusalBody(status int) map[string]any {
	switch status {
	case http.StatusNotFound:
		return refusal("input_not_given")
	case http.StatusForbidden, http.StatusGone:
		return refusal("input_expired")
	}
	return nil
}

func inputRefusal(status int) string {
	switch status {
	case http.StatusNotFound:
		return "this run was given no payload — TOFT_INPUT_URI points at an object " +
			"that was never written. A run started with parameters alone has no input; " +
			"read Run.Params for those. (A current platform leaves Run.InputURI empty in " +
			"that case, so an install still exporting it is older than this SDK.)"
	case http.StatusForbidden, http.StatusGone:
		return "this run's payload is no longer readable: the presigned link the platform " +
			"minted for it has expired. Read the input at the start of a run rather than " +
			"hours into one."
	}
	return fmt.Sprintf("reading this run's input failed: HTTP %d", status)
}

// ReadInputJSON is [Run.ReadInput] decoded into value.
func (r *Run) ReadInputJSON(ctx context.Context, value any) error {
	raw, err := r.ReadInput(ctx)
	if err != nil || len(raw) == 0 {
		return err
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return &Error{Message: fmt.Sprintf("this run's input is not the shape this job expects: %v", err), Wrapped: err}
	}
	return nil
}

// WriteOutput uploads this run's output.
//
// Also over a presigned URL, and also in both execution modes — a filesystem path
// would work in shared mode and would then make an app written against it unable
// to move to isolated mode.
func (r *Run) WriteOutput(ctx context.Context, body []byte) error {
	if r.OutputURI == "" {
		return &Error{Body: refusal("no_output"), Message: "this run has no output location"}
	}
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, r.OutputURI, bytes.NewReader(body))
	if err != nil {
		return &Error{Message: err.Error(), Wrapped: err}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &Error{Message: fmt.Sprintf("uploading output: %v", err), Wrapped: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &Error{Message: fmt.Sprintf("uploading output: %s", resp.Status), Status: resp.StatusCode}
	}
	return nil
}

// WriteOutputJSON is [Run.WriteOutput] with value encoded as JSON.
func (r *Run) WriteOutputJSON(ctx context.Context, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return &Error{Message: err.Error(), Wrapped: err}
	}
	return r.WriteOutput(ctx, encoded)
}

func (r *Run) call(ctx context.Context, path string, body any) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, &Error{Message: err.Error(), Wrapped: err}
	}
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, &Error{Message: err.Error(), Wrapped: err}
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Toft-Runner-Version", runnerContractVersion)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("cannot reach Toft at %s: %v", r.base, err), Wrapped: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fromResponse(resp)
	}
	return io.ReadAll(resp.Body)
}
