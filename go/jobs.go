package toft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Starting background work from inside an app.
//
// This is an app starting **a different entrypoint of itself**. Same repo, same
// release, same storage, different command — toft.toml declares it, and the
// platform runs it as its own process with a memory limit, a deadline and a
// durable record:
//
//	func export(w http.ResponseWriter, r *http.Request) {
//		run, err := toft.Jobs.Start(r.Context(), "reconcile", toft.StartOptions{
//			Params: map[string]any{"month": "2026-03"},
//		})
//		if err != nil {
//			http.Error(w, err.Error(), http.StatusBadGateway)
//			return
//		}
//		toft.Jobs.Respond(w, r, run) // 202 + Location, or the live status page
//	}
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
// **A handler responds; it never waits.** [JobsClient.Start] returns as soon as
// the run is queued, and [JobsClient.Respond] answers the browser with the run's
// status page. [JobRun.Wait] is for scripts and jobs: inside a request the
// gateway cuts the connection at its deadline, and a handler that waited past
// it gave its caller a bare 504 while the work went on unseen. So a wait that
// cannot finish before the request's deadline is refused at once with
// would_outlive_request rather than discovered at the gateway.
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
// **A run ends exactly once.** Nothing re-runs by itself: a failure is one run
// with its reason in [JobRun.FailureReason], and running it again is the app
// calling Start again — deciding whether and when — or a person pressing Run
// again in Toft.

const (
	jobsPath    = "/api/v1/app/jobs"
	jobRunsPath = "/api/v1/app/job-runs"

	// RequestDeadlineHeader is when the request an app is serving will be cut
	// off, RFC 3339. The gateway sets it on every request it forwards and on
	// nothing else, so its presence is also how the SDK knows it is serving one.
	RequestDeadlineHeader = "X-Toft-Request-Deadline"
)

// The eight words a run's status takes. Everything but queued and running is
// final.
const (
	RunQueued      = "queued"
	RunRunning     = "running"
	RunSucceeded   = "succeeded"
	RunFailed      = "failed"
	RunTimedOut    = "timed_out"
	RunOutOfMemory = "out_of_memory"
	RunCancelled   = "cancelled"
	RunSkipped     = "skipped"
)

// pollInterval is how often Wait and Tail re-read a run. A variable so the
// tests need not sleep for it.
var pollInterval = 2 * time.Second

// JobRun is a run, and everything an app can do with one.
//
// Returned by [JobsClient.Start] before the work begins — which is the point.
// Every method is a fresh read or an action; nothing is cached, because a run's
// whole purpose is to change while you are not looking. The JSON names are the
// platform's, so a JobRun encodes back into the body [JobsClient.Respond] sends.
type JobRun struct {
	ID     string `json:"id"`
	Job    string `json:"job"`
	Status string `json:"status"`

	// StartedBy is how the run began: "app", "viewer", "cli", "schedule" or
	// "ui". Never *which* person: an app does not learn that.
	StartedBy string `json:"started_by"`

	// RerunOf is the run this one repeats, when a person pressed Run again.
	RerunOf string `json:"rerun_of,omitempty"`

	// Params are the run's declared parameters with defaults applied — its only
	// input.
	Params map[string]any `json:"params,omitempty"`

	// ScheduledFor is the instant this run is for — its schedule's tick, or one a
	// person named — and nil when nobody gave one.
	ScheduledFor *time.Time `json:"scheduled_for,omitempty"`
	QueuedAt     time.Time  `json:"queued_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	// Deadline is when the run will be stopped if it is still going.
	Deadline *time.Time `json:"deadline,omitempty"`

	Progress        int    `json:"progress"`
	ProgressMessage string `json:"progress_message,omitempty"`

	// QueuePosition is 1-based while queued and 0 once the run starts. "3rd in
	// line" is most of what somebody wants when they ask about a queue.
	QueuePosition int `json:"queue_position,omitempty"`

	HasOutput bool `json:"has_output"`
	ExitCode  *int `json:"exit_code,omitempty"`
	// FailureReason is why a run that did not succeed ended, in words —
	// "exit 3: connection refused", "timed out after 45m", "stopped at 1024 MB".
	FailureStage  string `json:"failure_stage,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
	PeakMemoryMB  *int   `json:"peak_memory_mb,omitempty"`

	// StatusURL is the run's live status page on the app's own origin, which
	// [JobsClient.Respond] points a browser at. LogsURL is its log on the app
	// surface.
	StatusURL string `json:"status_url"`
	LogsURL   string `json:"logs_url"`

	// Deduplicated is true when this start returned a run that already existed.
	Deduplicated bool `json:"deduplicated,omitempty"`

	headers map[string]string
	// requestDeadline is the deadline of the request whose viewer started or
	// read this run, so Wait can refuse to outlive it without being told twice.
	requestDeadline time.Time
}

// Done reports whether this run will change again.
func (r JobRun) Done() bool { return r.Status != "" && r.Status != RunQueued && r.Status != RunRunning }

// Succeeded reports the happy terminal status.
func (r JobRun) Succeeded() bool { return r.Status == RunSucceeded }

// OutOfResources reports a run stopped for passing its limits, rather than one
// that crashed.
//
// Worth telling apart because the fix is different: a bigger limit or less work,
// not a code change.
func (r JobRun) OutOfResources() bool {
	return r.Status == RunOutOfMemory || r.Status == RunTimedOut
}

// Refresh re-reads this run and returns a fresh handle.
func (r JobRun) Refresh(ctx context.Context) (JobRun, error) {
	run, err := readRun(ctx, r.ID, r.headers)
	run.requestDeadline = r.requestDeadline
	return run, err
}

// WaitOptions bounds a [JobRun.Wait].
type WaitOptions struct {
	// Timeout is how long to wait; zero is until the run ends. Inside a request
	// it must fit before the request's deadline — see [JobRun.Wait].
	Timeout time.Duration

	// Request, or Header, is the request this code is serving, when it is
	// serving one. Either carries the gateway's X-Toft-Request-Deadline, which
	// is what lets Wait refuse a wait the gateway would cut short. A run started
	// through [ViewerFor] already carries it.
	Request *http.Request
	Header  http.Header

	// PollInterval is how often to re-read the run; zero is two seconds.
	PollInterval time.Duration
}

// Wait blocks until the run ends and returns it in its final state.
//
// For scripts, one-shot tools and jobs. **Inside a request it is refused at
// once** — code would_outlive_request, not retryable — once it knows the
// request: a run started through [ViewerFor] carries its deadline, and
// WaitOptions{Request: r} or {Header: h} names one. With neither it cannot tell
// a handler from a script. It is refused when it has no Timeout,
// or a Timeout past what is left of the request: the gateway would cut the
// connection at its deadline and the browser would get a bare 504. The fix is
// in the refusal's hint: respond with [JobsClient.Respond] and let the browser
// follow the run. A short wait that fits is allowed.
//
// A Timeout that runs out is named wait_timeout and is retryable, because the
// run is still going and waiting again is fair. Cancel the context to stop
// waiting; the run keeps going either way.
func (r JobRun) Wait(ctx context.Context, opts WaitOptions) (JobRun, error) {
	deadline := r.requestDeadline
	if opts.Request != nil {
		deadline = requestDeadline(opts.Request.Header)
	} else if opts.Header != nil {
		deadline = requestDeadline(opts.Header)
	}
	if !deadline.IsZero() {
		left := time.Until(deadline)
		if opts.Timeout <= 0 || opts.Timeout >= left {
			return r, wouldOutliveRequest(r, opts.Timeout, left)
		}
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	every := opts.PollInterval
	if every <= 0 {
		every = pollInterval
	}

	current := r
	// Named, so a caller can branch on it: a deadline is the run still going,
	// and waiting again is a fair answer — hence retryable.
	cutShort := func() error {
		body := map[string]any{"code": "wait_timeout", "retryable": true}
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			body = refusal("wait_cancelled")
		}
		return &Error{
			Message: fmt.Sprintf("run %s was still %s when the wait was cut short", r.ID, current.Status),
			Body:    body,
			Wrapped: ctx.Err(),
		}
	}
	for {
		refreshed, err := current.Refresh(ctx)
		if err != nil {
			// A deadline that lands mid-poll surfaces as the request's
			// failure; it is the same cut-short wait as one between polls.
			if ctx.Err() != nil {
				return current, cutShort()
			}
			// A poll the app's own rate limit refused is not the run failing:
			// wait out Retry-After and read again. Every other app call spends
			// the same limiter, so a busy app hits it mid-wait.
			pause, limited := rateLimitPause(err, every)
			if !limited {
				return current, err
			}
			select {
			case <-ctx.Done():
				return current, cutShort()
			case <-time.After(pause):
			}
			continue
		}
		current = refreshed
		if current.Done() {
			return current, nil
		}
		select {
		case <-ctx.Done():
			return current, cutShort()
		case <-time.After(every):
		}
	}
}

// rateLimitPause reports whether err is the app's rate limit refusing a poll,
// and how long to wait before the next one: what Retry-After asked, else one
// poll interval.
func rateLimitPause(err error, every time.Duration) (time.Duration, bool) {
	var e *Error
	if !errors.As(err, &e) || e.Code() != "rate_limited" {
		return 0, false
	}
	if e.RetryAfter > 0 {
		return e.RetryAfter, true
	}
	return every, true
}

// wouldOutliveRequest is the refusal for a wait the gateway would cut short.
func wouldOutliveRequest(r JobRun, timeout, left time.Duration) error {
	asked := "no timeout"
	if timeout > 0 {
		asked = "a timeout of " + timeout.String()
	}
	return &Error{
		Message: fmt.Sprintf("waiting on run %s with %s would outlive the request this code is "+
			"serving, which the gateway cuts off in %s — the caller would get a bare 504. "+
			"Respond with the run and let the browser follow it.", r.ID, asked, left.Round(time.Second)),
		Body: map[string]any{"code": "would_outlive_request", "retryable": false,
			"hint": "return toft.Jobs.Respond(w, r, run)"},
	}
}

// requestDeadline reads the gateway's deadline off a request's headers, or the
// zero time when there is none — outside a request, or reached around the
// gateway, where nothing will cut the connection.
func requestDeadline(headers http.Header) time.Time {
	raw := strings.TrimSpace(findHeader(headers, RequestDeadlineHeader))
	if raw == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return at
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
	return t.follow(ctx, fmt.Sprintf("%s/%s/output", jobRunsPath, url.PathEscape(r.ID)), r.headers)
}

// OutputJSON decodes the output WriteOutputJSON wrote into v, and reports false
// when the run produced none. The reading twin every caller otherwise writes by
// hand.
func (r JobRun) OutputJSON(ctx context.Context, v any) (bool, error) {
	raw, err := r.Output(ctx)
	if err != nil || len(raw) == 0 {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// LogLine is one line of a run's captured output.
type LogLine struct {
	Seq    int    `json:"seq"`
	Line   string `json:"line"`
	Stream string `json:"stream"`
	At     string `json:"at"`
}

// Logs reads this run's captured output after the line numbered after; 0 is
// from the start.
func (r JobRun) Logs(ctx context.Context, after int) ([]LogLine, error) {
	t, err := client()
	if err != nil {
		return nil, err
	}
	raw, err := t.get(ctx, fmt.Sprintf("%s/%s/logs?after=%d", jobRunsPath, url.PathEscape(r.ID), after), r.headers)
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

// Tail hands each new log line to visit until the run ends, or until visit
// returns false.
//
// A callback rather than a channel: a channel would need its own goroutine and a
// second way to stop it, and the caller already has a context.
func (r JobRun) Tail(ctx context.Context, visit func(LogLine) bool) error {
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
		keep, err := drain()
		var current JobRun
		if err == nil && keep {
			current, err = r.Refresh(ctx)
		}
		if pause, limited := rateLimitPause(err, pollInterval); limited {
			select {
			case <-ctx.Done():
				return &Error{Message: ctx.Err().Error(), Wrapped: ctx.Err()}
			case <-time.After(pause):
			}
			continue
		}
		if err != nil || !keep {
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
// A run a viewer started needs that viewer to cancel it — the handle from
// their [Viewer] carries them; the app acting as itself may cancel a run with
// no person behind it. Cancelling through the wrong one of those fails rather
// than quietly doing nothing.
func (r JobRun) Cancel(ctx context.Context) error {
	t, err := client()
	if err != nil {
		return err
	}
	_, err = t.postJSON(ctx, fmt.Sprintf("%s/%s/cancel", jobRunsPath, url.PathEscape(r.ID)), nil, "", r.headers)
	return err
}

func readRun(ctx context.Context, id string, headers map[string]string) (JobRun, error) {
	if strings.TrimSpace(id) == "" {
		return JobRun{}, badRequest("a run id is needed to read a run")
	}
	t, err := client()
	if err != nil {
		return JobRun{}, err
	}
	raw, err := t.get(ctx, jobRunsPath+"/"+url.PathEscape(id), headers)
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
	run.headers = headers
	return run, nil
}

// badRequest is a call the SDK refuses before it reaches the platform, coded
// as the platform codes the same mistake.
func badRequest(message string) error {
	return &Error{Body: refusal("bad_request"), Message: message}
}

// StartOptions carries what a run is started with.
type StartOptions struct {
	// Params are the run's only input, checked against the types, choices and
	// required flags toft.toml declares. They are recorded against the run, so
	// they must not carry secrets — those belong in the app's managed
	// environment. Large input goes into the app's storage first, and its name
	// is passed here.
	Params map[string]any

	// IdempotencyKey makes a repeated start return the first run for 24 hours.
	// Without one the platform derives a key from the viewer and the params,
	// good for a minute — right for a double click, and the case that bites.
	IdempotencyKey string
}

// ListOptions narrows [JobsClient.List] and [JobsClient.Mine].
type ListOptions struct {
	// Job is one job's runs; "" is every job's.
	Job string
	// Status is runs in one status; "" is every status.
	Status string
	// Limit is how many, newest first; zero is 50 for List and 20 for Mine.
	Limit int
}

// JobsClient is the job surface, either as the app or on a person's behalf.
//
// [Jobs] is the app acting as itself. [Viewer.Jobs] is the same surface with a
// viewer attached, and the difference is one relayed header — which the app
// cannot mint, alter, or read.
type JobsClient struct {
	headers map[string]string
	// deadline is the request deadline of the viewer this client came from,
	// handed on to every run it returns.
	deadline time.Time
}

// Start queues a run and returns its handle immediately.
func (j *JobsClient) Start(ctx context.Context, job string, opts StartOptions) (JobRun, error) {
	if strings.TrimSpace(job) == "" {
		return JobRun{}, badRequest("Start needs the name of a job declared in toft.toml")
	}
	var body any
	if len(opts.Params) > 0 {
		body = map[string]any{"params": opts.Params}
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
	run, err := runFrom(raw, j.headers)
	run.requestDeadline = j.deadline
	return run, err
}

// Respond answers the request that started run, and is what a handler returns
// instead of waiting.
//
// A browser — a request whose Accept names text/html — is sent 303 See Other to
// the run's live status page, which the gateway serves on the app's own origin:
// the job's name, its progress and message, the outcome and its reason, styled
// with no app code. Anything else gets 202 Accepted, Location: the status page,
// and the run as JSON, which is how a script or the app's own frontend learns
// where to poll.
//
// The error is only ever the body's encoding; the status line has already been
// written by then.
func (j *JobsClient) Respond(w http.ResponseWriter, r *http.Request, run JobRun) error {
	if run.StatusURL != "" {
		w.Header().Set("Location", run.StatusURL)
	}
	if r != nil && run.StatusURL != "" && strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
		w.WriteHeader(http.StatusSeeOther)
		return nil
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	return json.NewEncoder(w).Encode(run)
}

// Get reads one run by id.
func (j *JobsClient) Get(ctx context.Context, runID string) (JobRun, error) {
	run, err := readRun(ctx, runID, j.headers)
	run.requestDeadline = j.deadline
	return run, err
}

// List is this app's runs, newest first, narrowed by opts.
func (j *JobsClient) List(ctx context.Context, opts ListOptions) ([]JobRun, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	return j.list(ctx, "", opts)
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
func (j *JobsClient) Mine(ctx context.Context, opts ListOptions) ([]JobRun, error) {
	if len(j.headers) == 0 {
		return nil, &Error{Message: "Mine needs a viewer — call it on toft.ViewerFor(r.Header).Jobs. " +
			"The app acting as itself has no viewer to scope to, and answering with every run " +
			"would be a silent widening rather than an answer.",
			Body: map[string]any{"code": "no_viewer", "retryable": false,
				"hint": "use toft.Jobs.List(...) for every run of this app"}}
	}
	if opts.Limit <= 0 {
		opts.Limit = 20
	}
	return j.list(ctx, "viewer", opts)
}

func (j *JobsClient) list(ctx context.Context, scope string, opts ListOptions) ([]JobRun, error) {
	query := url.Values{"limit": {strconv.Itoa(opts.Limit)}}
	if scope != "" {
		query.Set("scope", scope)
	}
	if opts.Job != "" {
		query.Set("job", opts.Job)
	}
	if opts.Status != "" {
		query.Set("status", opts.Status)
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
		body.Runs[i].requestDeadline = j.deadline
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
