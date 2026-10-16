package toft

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Starting background work from inside an app.
//
// This is an app starting **a different entrypoint of itself**. Same repo, same
// release, same storage, different command — toft.toml declares it, and the
// platform runs it as an isolated workload with a memory limit, a deadline and a
// durable record:
//
//	// The app acting as itself — no viewer, no actor.
//	run, err := toft.Jobs.Start(ctx, "reconcile", toft.StartOptions{
//		Params: map[string]any{"month": "2026-03"},
//	})
//
//	// The app acting for whoever is looking at the page.
//	me, err := toft.ViewerFor(r.Header)
//	run, err := me.Jobs.Start(ctx, "reconcile", toft.StartOptions{Params: params})
//
// Which object you call is what says whether a run is attributed to a person,
// and that is deliberate. A flag would make the anonymous case the default
// spelling of the attributed one, and attribution would then be lost by omission
// — a run sliding quietly out of somebody's name and into nobody's, noticed at
// the next audit if at all.
//
// **Nothing here blocks on the work.** [JobsClient.Start] returns as soon as the
// run is queued. [JobRun.Wait] exists, is for scripts and one-shot tools, and is
// never what Start does — an isolated workload's cold start alone is measured in
// tens of seconds, so a call that could return a result would be a call that
// sometimes holds an HTTP handler for a minute.
//
// **A repeated start is not a second run.** The platform derives an idempotency
// key from the app, the job, the parameters and the resolved person, and a start
// matching a run already in flight — or one that finished in the last minute —
// comes back as *that* run with [JobRun.Deduplicated] set. Reported, never
// silent: an app that appeared to ignore a click is indistinguishable from a
// broken one.
//
// **Coming back tomorrow works, and the app stores nothing to make it work.**
// [JobsClient.Mine] asks the platform for this viewer's runs. The app relays the
// act-for token; the platform resolves it and filters on the person it resolved.
// The app gets that person's runs and still never learns who they are.
//
// **A run you start may be retried, and this object follows the chain.** Retries
// are off by default and switched on per job — on the app's Automation tab, or as
// retries = N in toft.toml — never in code here. What the SDK does is make
// sure the chain is not something you have to know about:
//
//   - [JobRun.Done] is **false** on a failed run the platform has already queued
//     another attempt at. The work is still in progress, in a row with a
//     different id, and reporting that as finished would be a failure announced
//     for something about to succeed.
//   - [JobRun.Refresh] returns the *newer attempt* when there is one, so
//     [JobRun.Wait] resolves on the outcome of the work rather than of the first
//     try at it.
//   - [JobRun.Attempt] is which try you are looking at, and [JobRun.RetriedBy]
//     names the successor when one exists. Read them when the distinction
//     matters; ignore them and the defaults are right.
//
// Which runs are eligible is the platform's rule, not this library's: a schedule
// and anything the app started — including a run started on a viewer's behalf,
// since the app fired it and did not block on it. A run somebody started from the
// Toft panel is never retried behind them, and neither is one that ran out
// of time or memory, because a second attempt would end the same way.

const (
	jobsPath    = "/api/v1/app/jobs"
	jobRunsPath = "/api/v1/app/job-runs"
)

// terminalRunStatuses will not change again. Polling stops here.
var terminalRunStatuses = map[string]bool{
	"succeeded": true, "failed": true, "cancelled": true,
	"timed_out": true, "resource_exceeded": true, "infrastructure_error": true,
}

// JobRun is a run, and everything an app can do with one.
//
// Returned by [JobsClient.Start] before the work begins — which is the point.
// Every method is a fresh read or an action; nothing is cached, because a run's
// whole purpose is to change while you are not looking.
type JobRun struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	JobName  string `json:"job_name"`
	Progress int    `json:"progress"`

	// QueuePosition is 1-based while pending and 0 once the run starts. "3rd in
	// line" is most of what somebody wants when they ask about a queue.
	QueuePosition int `json:"queue_position"`

	// Deduplicated is true when this start returned a run that already existed.
	Deduplicated bool `json:"deduplicated"`

	// StartedByViewer is true when the run was started on a person's behalf
	// rather than by the app itself. Never says *which* person: an app does not
	// learn that.
	StartedByViewer bool `json:"started_by_viewer"`

	HasOutput     bool   `json:"has_output"`
	ExitCode      *int   `json:"exit_code"`
	FailureStage  string `json:"failure_stage"`
	FailureReason string `json:"failure_reason"`

	// Result is the structured result the job recorded with [Run.SetResult]. The
	// smallest of the result channels and the right one for what most internal
	// tools produce: counts, a summary, a few hundred rows.
	Result any `json:"result"`

	// Attempt is which try this run is, and RetriedBy names the run that
	// superseded it when the platform queued another attempt. A failed run with
	// RetriedBy set is not the end of the story — see [JobRun.Wait].
	Attempt   int    `json:"attempt"`
	RetriedBy string `json:"retried_by"`
	// ScheduledFor is the instant this run is for — its schedule's tick, or
	// one a person named — as RFC 3339, or "" when nobody gave one.
	ScheduledFor string `json:"scheduled_for,omitempty"`

	headers map[string]string
}

// Done reports whether this run will change again.
//
// A failed run the platform has already queued another attempt at is *not* done:
// the work is still in progress, in a row with a different id. Reading only this
// row's status there would report a failure for something that is about to
// succeed.
func (r JobRun) Done() bool { return terminalRunStatuses[r.Status] && r.RetriedBy == "" }

// Succeeded reports the happy terminal status.
func (r JobRun) Succeeded() bool { return r.Status == "succeeded" }

// OutOfResources reports a run killed for exceeding its limits, rather than one
// that crashed.
//
// Worth telling apart because the fix is different: a bigger limit or less work,
// not a code change.
func (r JobRun) OutOfResources() bool {
	return r.Status == "resource_exceeded" || r.Status == "timed_out"
}

// Refresh re-reads this run and returns a fresh handle.
//
// **It follows a retry.** If the platform has queued another attempt at this
// work, the handle you get back is that attempt — same chain, newer row. An app
// that started a run and waits on it should see the outcome of the work, not of
// the first try at it; [JobRun.Attempt] is there for anyone who needs the
// distinction.
func (r JobRun) Refresh(ctx context.Context) (JobRun, error) {
	run, err := readRun(ctx, r.ID, r.headers)
	if err != nil || run.RetriedBy == "" {
		return run, err
	}
	return readRun(ctx, run.RetriedBy, r.headers)
}

// Wait blocks until the run finishes.
//
// For one-shot tools and scripts. **Never call this on an HTTP handler's path** —
// that is the twelve-minute block this whole feature exists to remove. Cancel the
// context to stop waiting; the run keeps going.
func (r JobRun) Wait(ctx context.Context, pollInterval time.Duration) (JobRun, error) {
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	current := r
	for {
		refreshed, err := current.Refresh(ctx)
		if err != nil {
			return current, err
		}
		current = refreshed
		if current.Done() {
			return current, nil
		}
		select {
		case <-ctx.Done():
			return current, &Error{
				Message: fmt.Sprintf("run %s was still %s when the wait was cut short", r.ID, current.Status),
				Wrapped: ctx.Err(),
			}
		case <-time.After(pollInterval):
		}
	}
}

// Output fetches this run's output object, or nil if it produced none.
//
// Over a presigned URL the platform mints — the app holds no object-store
// credential, because one that could read this run's output could read every
// other app's data.
//
// It fails when the artifact *did* exist and has since expired. An output object
// lives exactly as long as the run row that explains it, and saying so is better
// than a download that quietly 404s: a run still reading succeeded with nothing
// behind it is the worst failure shape available here.
func (r JobRun) Output(ctx context.Context) ([]byte, error) {
	t, err := client()
	if err != nil {
		return nil, err
	}
	return t.follow(ctx, fmt.Sprintf("%s/%s/output", jobRunsPath, r.ID), r.headers)
}

// LogLine is one line of a run's captured output.
type LogLine struct {
	Seq    int    `json:"seq"`
	Line   string `json:"line"`
	Stream string `json:"stream"`
	At     string `json:"at"`
}

// Logs reads this run's captured output from afterSeq onwards.
func (r JobRun) Logs(ctx context.Context, afterSeq int) ([]LogLine, error) {
	t, err := client()
	if err != nil {
		return nil, err
	}
	raw, err := t.get(ctx, fmt.Sprintf("%s/%s/logs?after=%d", jobRunsPath, r.ID, afterSeq), r.headers)
	if err != nil {
		return nil, err
	}
	var body struct {
		Lines []LogLine `json:"lines"`
	}
	if err := decode(raw, &body); err != nil {
		return nil, err
	}
	return body.Lines, nil
}

// Tail hands each new log line to visit until the run reaches a terminal status,
// or until visit returns false.
//
// A callback rather than a channel: a channel would need its own goroutine and a
// second way to stop it, and the caller already has a context.
func (r JobRun) Tail(ctx context.Context, pollInterval time.Duration, visit func(LogLine) bool) error {
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	seq := 0
	drain := func() (bool, error) {
		lines, err := r.Logs(ctx, seq)
		if err != nil {
			return false, err
		}
		for _, line := range lines {
			if line.Seq > seq {
				seq = line.Seq
			}
			if !visit(line) {
				return false, nil
			}
		}
		return true, nil
	}
	for {
		if keep, err := drain(); err != nil || !keep {
			return err
		}
		current, err := r.Refresh(ctx)
		if err != nil {
			return err
		}
		if current.Done() {
			// One last read: lines can land between the poll and the status
			// check, and losing the end of a failing job's output loses the part
			// that explains it.
			_, err := drain()
			return err
		}
		select {
		case <-ctx.Done():
			return &Error{Message: ctx.Err().Error(), Wrapped: ctx.Err()}
		case <-time.After(pollInterval):
		}
	}
}

// Cancel stops this run.
//
// A viewer may cancel a run they started and not somebody else's; the app acting
// as itself may cancel a run with no person behind it. Cancelling through the
// wrong one of those fails rather than quietly doing nothing.
func (r JobRun) Cancel(ctx context.Context) error {
	t, err := client()
	if err != nil {
		return err
	}
	_, err = t.postJSON(ctx, fmt.Sprintf("%s/%s/cancel", jobRunsPath, r.ID), nil, "", r.headers)
	return err
}

func readRun(ctx context.Context, id string, headers map[string]string) (JobRun, error) {
	t, err := client()
	if err != nil {
		return JobRun{}, err
	}
	raw, err := t.get(ctx, jobRunsPath+"/"+id, headers)
	if err != nil {
		return JobRun{}, err
	}
	return runFrom(raw, headers)
}

func runFrom(raw []byte, headers map[string]string) (JobRun, error) {
	var run JobRun
	if err := decode(raw, &run); err != nil {
		return JobRun{}, err
	}
	if run.Status == "" {
		run.Status = "pending"
	}
	if run.Attempt == 0 {
		run.Attempt = 1
	}
	run.headers = headers
	return run, nil
}

// StartOptions carries what a run is started with.
type StartOptions struct {
	// Params are recorded against the run for reproducibility and audit, so they
	// must not carry secrets — those belong in the app's managed environment,
	// which a run references rather than copies.
	Params map[string]any

	// Payload is bulk input, stored in the object store and handed to the run as
	// a presigned URL; the job reads it with [Run.ReadInput].
	Payload any

	// IdempotencyKey overrides the derived one. The derivation is already
	// correct for the case that actually bites, so pass one only when the app has
	// a better notion of "the same request" than the parameters give.
	IdempotencyKey string
}

// JobsClient is the job surface, either as the app or on a person's behalf.
//
// [Jobs] is the app acting as itself. [Viewer.Jobs] is the same surface with a
// viewer attached, and the difference is one relayed header — which the app
// cannot mint, alter, or read.
type JobsClient struct {
	headers map[string]string
}

// Start queues a run and returns its handle immediately.
func (j *JobsClient) Start(ctx context.Context, job string, opts StartOptions) (JobRun, error) {
	body := map[string]any{}
	if len(opts.Params) > 0 {
		body["params"] = opts.Params
	}
	if opts.Payload != nil {
		body["payload"] = opts.Payload
	}
	t, err := client()
	if err != nil {
		return JobRun{}, err
	}
	raw, err := t.postJSON(ctx, fmt.Sprintf("%s/%s/runs", jobsPath, url.PathEscape(job)),
		body, opts.IdempotencyKey, j.headers)
	if err != nil {
		return JobRun{}, typedQueueFull(err)
	}
	return runFrom(raw, j.headers)
}

// Get reads one run by id.
func (j *JobsClient) Get(ctx context.Context, runID string) (JobRun, error) {
	return readRun(ctx, runID, j.headers)
}

// List is every run of this app, newest first. A jobName of "" is all of them.
func (j *JobsClient) List(ctx context.Context, jobName string, limit int) ([]JobRun, error) {
	return j.list(ctx, "app", jobName, limit)
}

// Mine is this viewer's runs of this app, newest first.
//
// Only on a [Viewer] — the app acting as itself has no "mine" to ask about, and
// calling it there fails rather than quietly returning everything.
//
// This is what makes coming back tomorrow work without the app storing a single
// per-user row. The run is a durable record on the platform keyed on the
// *person*, so she can start it on a laptop and check it from a phone.
// Authorization stays live too: if she lost read on the app overnight the list is
// empty, because the filter runs against a principal the platform resolved itself.
func (j *JobsClient) Mine(ctx context.Context, jobName string, limit int) ([]JobRun, error) {
	if len(j.headers) == 0 {
		return nil, &Error{Message: "Mine needs a viewer — call it on toft.ViewerFor(r.Header).Jobs. " +
			"The app acting as itself has no viewer to scope to, and answering with every run " +
			"would be a silent widening rather than an answer."}
	}
	return j.list(ctx, "viewer", jobName, limit)
}

func (j *JobsClient) list(ctx context.Context, scope, jobName string, limit int) ([]JobRun, error) {
	if limit <= 0 {
		limit = 50
	}
	query := url.Values{"scope": {scope}, "limit": {strconv.Itoa(limit)}}
	if jobName != "" {
		query.Set("job", jobName)
	}
	t, err := client()
	if err != nil {
		return nil, err
	}
	raw, err := t.get(ctx, jobRunsPath+"?"+query.Encode(), j.headers)
	if err != nil {
		return nil, err
	}
	var body struct {
		Runs []JobRun `json:"runs"`
	}
	if err := decode(raw, &body); err != nil {
		return nil, err
	}
	for i := range body.Runs {
		body.Runs[i].headers = j.headers
		if body.Runs[i].Attempt == 0 {
			body.Runs[i].Attempt = 1
		}
	}
	return body.Runs, nil
}

// typedQueueFull promotes a queue-full refusal to the error a caller can render.
//
// The platform sends the counts; flattening them into "429 Too Many Requests"
// throws away the only part an app could put on a page.
func typedQueueFull(err error) error {
	platform, ok := AsError(err)
	if !ok || platform.Status != 429 || platform.Code() != "queue_full" {
		return err
	}
	number := func(key string) int {
		value, _ := platform.Body[key].(float64)
		return int(value)
	}
	return &QueueFullError{Err: platform, InFlight: number("in_flight"), Limit: number("limit")}
}
