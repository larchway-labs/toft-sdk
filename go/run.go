package toft

import (
	"bytes"
	"context"
	"encoding/json"
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
//	run.Progress(ctx, 80, "writing the CSV")
//	run.WriteOutputJSON(ctx, map[string]any{"rows": len(rows), "file": "reconcile/2026-03.csv"})
//
// **Params are the only input, and the output is the one way back.** Large
// input goes into the app's storage before the start and its name travels as a
// param; a large result goes into storage too, and the output names it. A job
// does not reach toft.Data — its result comes back through its output, the
// app's storage or the app's own database.
//
// **Never write a durable result into scratch.** TOFT_SCRATCH_DIR is wiped
// on every start and [SessionScratchDir] is per-login, so a report written there
// is a dead path tomorrow while the run row cheerfully still says succeeded.
// Status outliving its payload is the worst failure shape available here, which
// is why an output should carry a storage-relative reference rather than a raw
// path.

// runnerContractVersion is the callback contract this SDK speaks, and it must
// equal the platform's runner.ContractVersion. A partial upgrade is reported
// rather than tolerated, so a stale number here is a 409 on every callback a job
// makes. sdk/contract_test.go keeps the four copies of it in step.
const runnerContractVersion = "2"

// Run is the job's own view of itself: who it is, what it was given, and how it
// reports back.
//
// [CurrentRun] configures it from what the runner already exported, so a job
// never handles a credential explicitly.
type Run struct {
	// ID is the run this process is executing; Job is the job's name.
	ID  string
	Job string

	// RerunOf is the run this one repeats, when a person pressed Run again —
	// "" for a first run. A job that must not double-apply its work reads it.
	RerunOf string

	// Trigger is *how* this run started: "app", "viewer", "cli", "schedule" or
	// "ui". TriggerRole is *who* — one of [RoleOwner], [RoleEditor],
	// [RoleViewer], [RoleAnonymous], [RoleSystem] (a schedule firing on its
	// schedule) or [RoleApp] (the app starting work for itself), and
	// [RoleUnknown] where the platform did not say.
	//
	// The pair is the point. "ui" tells a report job nothing about whether to
	// include the private columns; "ui" plus "viewer" does.
	//
	// **The role is today's, not the day the run was queued.** It is derived
	// from the run's actor when the workload starts, so somebody demoted in
	// between reports their current role — which is the right answer for a job
	// deciding what work to do, and the wrong one for an audit. The audit
	// question is answered by the run's actor in Toft's own history, which
	// does not move.
	Trigger     string
	TriggerRole string

	// OutputURI is the presigned location the platform minted for this run's
	// output. A job holds no object-store credential, because one that could
	// write this run's output could reach every other app's.
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
	id := os.Getenv(EnvRunID)
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
		Job:         os.Getenv(EnvJobName),
		RerunOf:     os.Getenv(EnvRerunOf),
		Trigger:     os.Getenv("TOFT_TRIGGER"),
		TriggerRole: triggerRole,
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
// They arrive typed — declared defaults applied, an int an int — so a struct of
// the app's own reads them directly. Reading through a typed shape is also where
// a job notices a param it does not expect.
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
// was named, and a rerun carries the original's. Never defaulted to now: a re-run
// of Tuesday's work on Thursday must not quietly process Thursday.
func (r *Run) ScheduledFor() (time.Time, bool) {
	return envTime(EnvScheduledFor)
}

// Deadline is when the platform will stop this run, and false when it did not
// say. A job with a long tail of work checks it before starting a step it
// cannot finish, and leaves its output in a state the next run can resume from.
func (r *Run) Deadline() (time.Time, bool) {
	return envTime(EnvDeadline)
}

func envTime(name string) (time.Time, bool) {
	raw := os.Getenv(name)
	if raw == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// Progress reports how far along this run is, 0–100, with an optional short
// message — "loading rows" — shown beside the bar on the run's status page.
func (r *Run) Progress(ctx context.Context, percent int, message string) error {
	body := map[string]any{"version": runnerContractVersion, "percent": percent}
	if message != "" {
		body["message"] = message
	}
	_, err := r.call(ctx, "/api/v1/runner/progress", body)
	return err
}

// Log writes one line to the run's log: stdout, or stderr when isError. The
// runner captures both, so this is a print with the stream named — and the
// last line on stderr is what a failed run gives as its reason.
func (r *Run) Log(message string, isError bool) {
	out := os.Stdout
	if isError {
		out = os.Stderr
	}
	fmt.Fprintln(out, message)
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
