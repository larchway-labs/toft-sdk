package toft

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
)

// The scheduled task a request *is*, and the log the platform cannot capture.
//
//	func runReport(w http.ResponseWriter, r *http.Request) {
//		task := toft.TaskFor(r.Header) // nil when this is an ordinary visit
//		task.Log(r.Context(), fmt.Sprintf("reading %d rows", n))
//		...
//		task.Log(r.Context(), "wrote report.xlsx")
//	}
//
// **Why an app has to do this at all.** A background job's output belongs to the
// platform: the runner owns the process, captures its stdout and ships it. A
// scheduled task is an HTTP request against an endpoint the app published, so
// there is no platform-owned process to read — the task's writes land in the
// app's own log stream, interleaved with every other request being served at that
// moment, and cannot be sliced apart afterwards. Pushing lines is the only way a
// task's own log can exist, and it is opt-in because only the app knows which of
// its output belongs to this run.
//
// **What it is for.** A run that passes the platform's task timeout is recorded
// as a failure with *nothing* in it — no result and no body, because the app never
// answered. Lines pushed as the run goes are what is left: the log right up to the
// kill. That is the failure this exists for, and the reason it is worth a call in
// the app rather than a setting somewhere.
//
// **Nothing here can break the run.** A push that fails — the run already closed,
// the platform unreachable, the cap reached — is swallowed. Logging that could
// take down the thing it observes would be worse than no logging, so this returns
// false and gets on with it. **A nil [Task] is a working [Task]**, whose Log does
// nothing and says so: that is what lets the call sites above have no branch
// around them, and it is the same principle as [TaskFor] returning nil off a
// scheduled firing — a report endpoint somebody also opens in a browser must not
// fail because nobody scheduled that visit.
//
// **The app is not asserting anything.** The run id arrives in a header the
// platform set; what authorizes the push is the app's own token, and the platform
// scopes the run to the app that holds it. An app can log to its own runs and to
// nothing else, whatever it puts in the header.
//
// **A retry is a different run, with a log of its own.** If the task asks for
// retries — on its Automation tab, never in code here — a failed firing is tried
// again as a *new* run: your handler is called afresh, [TaskFor] hands back the
// new run id, and the lines you push land on that attempt. Nothing carries over,
// which is the point: each attempt's log is what that attempt did, and the failed
// one keeps its own.
//
// **Pushing to a closed run fails quietly, and that is the interesting case.** If
// your handler outlives the platform's task timeout, the run is already recorded
// as failed and stops accepting lines; [Task.Log] returns false and this object
// stops trying. The lines you pushed *before* the deadline were kept, which is
// the whole reason to push as you go rather than at the end.
//
// **A task need not have a schedule, and the marker is still set.** A task whose
// cron expression is empty is fired only on demand — `toft task run`, the Run now
// button, or the app asking for it by name — and it travels the one firing path
// every other firing takes, so `X-Toft-Cron: 1` is on the request either
// way. So [TaskFor] answers for an on-demand firing too, and reading the marker
// is *this was a firing*, never *a schedule came round*.

// Headers the platform sets on a scheduled trigger. CronHeader has been on every
// trigger since before tasks had names, and apps in the wild branch on it, so it
// stays the marker.
//
// ScheduleHeader carries **the task's name** — `refresh`, not `0 3 * * *`. That
// is the platform's deliberate choice and the useful one: one endpoint serving
// several schedules needs to know which fired, and a cron expression does not
// identify a task. The header keeps the spelling it has always had; [Task.Name]
// is what it is called here.
const (
	CronHeader     = "X-Toft-Cron"
	RunHeader      = "X-Toft-Run"
	ScheduleHeader = "X-Toft-Schedule"
)

const appRunsPath = "/api/v1/app/runs"

// Task is one run of a task, from inside the request that is running it.
//
// *A run of a task*, not *a scheduled run*: a task with no cron expression is
// fired on demand and arrives here the same way.
type Task struct {
	// ID is the run id, the same one the panel shows.
	ID string

	// Name is the name of the task that fired, so one endpoint serving several
	// can tell them apart without reading its own parameters back. The name an
	// administrator typed on the Automation tab — `refresh` — and never its cron
	// expression, which no call in this SDK gives an app.
	//
	// It was called Schedule, which is what the header is called, and the name
	// was the whole defect: an app rendering "last scheduled run: {Schedule}"
	// published *last scheduled run: refresh*. Renamed rather than aliased,
	// because an alias would keep that page wrong under a name that reads right.
	Name string

	// closed is set once the platform says the run is closed or full. After that
	// every further call is a no-op rather than a request that will be refused
	// identically — a task in a loop should not spend its remaining time
	// discovering the same 409.
	closed atomic.Bool
}

// TaskFor builds a [Task] from the request headers, or returns nil.
//
// nil means this request is not a scheduled firing — somebody opened the endpoint
// in a browser, a health check hit it, or the app is running outside the gateway.
// That is not an error and must not fail: the same endpoint serving both a
// schedule and a person is the ordinary shape of a report app. The nil is safe to
// call [Task.Log] on, so app code needs no branch.
//
// This mirrors [ViewerFor] in construction and differs in exactly that: a missing
// act-for header is an error, because a run sliding out of a person's name into
// nobody's is a fact somebody has to notice; a missing run header is just a
// visitor.
func TaskFor(headers http.Header) *Task {
	runID := findHeader(headers, RunHeader)
	if runID == "" {
		return nil
	}
	return &Task{ID: runID, Name: findHeader(headers, ScheduleHeader)}
}

// Log pushes one line onto this run's log. Never fails, and is safe on a nil
// [Task].
//
// Reports whether the line was stored, for the rare caller that wants to know;
// most should ignore it.
func (t *Task) Log(ctx context.Context, message string) bool {
	return t.LogLines(ctx, []string{message}, false)
}

// LogError is [Task.Log] onto the run's stderr stream.
func (t *Task) LogError(ctx context.Context, message string) bool {
	return t.LogLines(ctx, []string{message}, true)
}

// LogLines pushes several lines in one request.
//
// Batching is the app's choice, deliberately: a buffer here would mean lines held
// in memory when the process is killed, which is precisely the case this feature
// exists to serve.
func (t *Task) LogLines(ctx context.Context, messages []string, isError bool) bool {
	if t == nil || len(messages) == 0 || t.closed.Load() {
		return false
	}
	stream := "stdout"
	if isError {
		stream = "stderr"
	}
	lines := make([]map[string]string, 0, len(messages))
	for _, message := range messages {
		lines = append(lines, map[string]string{"line": message, "stream": stream})
	}

	transport, err := client()
	if err != nil {
		t.closed.Store(true)
		return false
	}
	raw, err := transport.postJSON(ctx, fmt.Sprintf("%s/%s/logs", appRunsPath, t.ID),
		map[string]any{"lines": lines}, "", nil)
	if err != nil {
		// A closed run answers 409 and a full one answers with dropped>0; both
		// are ordinary, and neither is the app's problem to handle.
		t.closed.Store(true)
		return false
	}
	var body struct {
		Stored  int `json:"stored"`
		Dropped int `json:"dropped"`
	}
	if err := decode(raw, &body); err != nil {
		return true
	}
	if body.Dropped > 0 {
		t.closed.Store(true)
		return body.Stored > 0
	}
	return true
}

// String is a debugging aid.
func (t *Task) String() string {
	if t == nil {
		return "<toft.Task (not a scheduled firing)>"
	}
	return fmt.Sprintf("<toft.Task %q run=%s>", t.Name, t.ID)
}
