/**
 * The scheduled task a request *is*, and the log the platform cannot capture.
 *
 *   app.get("/reports/run", async (req, res) => {
 *     const task = toft.task(req.headers);        // null on an ordinary visit
 *     await task?.log(`reading ${n} rows`);
 *     ...
 *     await task?.log("wrote report.xlsx");
 *     res.json({ rows: n });
 *   });
 *
 * **Why an app has to do this at all.** A background job's output belongs to
 * the platform: the runner owns the process, captures its stdout and ships it.
 * A scheduled task is an HTTP request against an endpoint the app published, so
 * there is no platform-owned process to read — the task's prints land in the
 * app's own log stream, interleaved with every other request being served at
 * that moment, and cannot be sliced apart afterwards. Pushing lines is the only
 * way a task's own log can exist, and it is opt-in because only the app knows
 * which of its prints belong to this run.
 *
 * **What it is for.** A run that passes the platform's task timeout is recorded
 * as a failure with *nothing* in it — no result and no body, because the app
 * never answered. Lines pushed as the run goes are what is left: the log right
 * up to the kill.
 *
 * **Nothing here can break the run.** A push that fails — the run already
 * closed, the platform unreachable, the cap reached — resolves `false` rather
 * than rejecting. Logging that could take down the thing it observes would be
 * worse than no logging. `toft.task(...)` returning `null` off a scheduled firing
 * is the same principle: a report endpoint somebody also opens in a browser
 * must not throw because nobody scheduled that visit.
 *
 * **The app is not asserting anything.** The run id arrives in a header the
 * platform set; what authorizes the push is the app's own token, and the
 * platform scopes the run to the app that holds it.
 *
 * **A retry is a different run, with a log of its own.** If the task asks for
 * retries — on its Automation tab, never in code here — a failed firing is
 * tried again as a *new* run: your handler is called afresh, `toft.task(headers)`
 * hands back the new run id, and the lines you push land on that attempt.
 * Nothing carries over, which is the point: each attempt's log is what that
 * attempt did, and the failed one keeps its own. Retries apply to firings
 * nobody is waiting on — the schedule, and ones the app triggers through `toft` —
 * while the Toft panel's own Run now is never retried behind the person
 * who pressed it.
 *
 * **Pushing to a closed run fails quietly, and that is the interesting case.**
 * If your handler outlives the platform's task timeout, the run is already
 * recorded as failed and stops accepting lines; `log()` resolves `false` and
 * this object stops trying. The lines you pushed *before* the deadline were
 * kept, which is the whole reason to push as you go rather than at the end.
 *
 * **A task need not have a schedule, and the marker is still set.** A task whose
 * cron expression is empty is fired only on demand — `toft task run`, the Run now
 * button, or the app asking for it by name — and it travels the one firing path
 * every other firing takes, so `X-Toft-Cron: 1` is on the request either
 * way. So `toft.task(headers)` is non-null for an on-demand firing too, and
 * reading the marker is *this was a firing*, never *a schedule came round*.
 */

import { transport } from "./transport.js";

/**
 * Headers the platform sets on a scheduled trigger. `X-Toft-Cron` has been
 * on every trigger since before tasks had names, and apps in the wild branch on
 * it, so it stays the marker.
 *
 * `X-Toft-Schedule` carries **the task's name** — `refresh`, not
 * `0 3 * * *`. That is the platform's deliberate choice and the useful one: one
 * endpoint serving several schedules needs to know which fired, and a cron
 * expression does not identify a task. The header keeps the spelling it has
 * always had; `Task.name` is what it is called here.
 */
export const CRON_HEADER = "X-Toft-Cron";
export const RUN_HEADER = "X-Toft-Run";
export const SCHEDULE_HEADER = "X-Toft-Schedule";

const RUNS_PATH = "/api/v1/app/runs";

/**
 * One run of a task, from inside the request that is running it.
 *
 * *A run of a task*, not *a scheduled run*: a task with no cron expression is
 * fired on demand and arrives here the same way.
 */
export class Task {
  constructor(runId, name) {
    /** The run id, the same one the panel shows. */
    this.id = runId;
    /**
     * **The name of the task that fired**, so one endpoint serving several can
     * tell them apart. The name an administrator typed on the Automation tab —
     * `refresh` — and never its cron expression, which no call in this SDK
     * gives an app.
     */
    this.name = name;
    // Set once the platform says the run is closed or full. Every further call
    // is then a no-op rather than a request that will be refused identically —
    // a task in a loop should not spend its remaining time rediscovering the
    // same 409.
    this._closed = false;
  }

  /** Push one line onto this run's log. Never rejects. */
  async log(message, { error = false } = {}) {
    return this.logLines([message], { error });
  }

  /**
   * Push several lines in one request.
   *
   * Batching is the app's choice, deliberately: a buffer here would mean lines
   * held in memory when the process is killed, which is precisely the case this
   * feature exists to serve.
   */
  async logLines(messages, { error = false } = {}) {
    if (this._closed || !messages?.length) return false;
    const stream = error ? "stderr" : "stdout";
    const lines = messages.map((m) => ({ line: String(m), stream }));
    try {
      const raw = await transport().postJson(`${RUNS_PATH}/${this.id}/logs`, { lines });
      if (raw?.dropped) {
        this._closed = true;
        return Boolean(raw?.stored);
      }
      return true;
    } catch {
      // A closed run answers 409 and a full one reports dropped lines; both are
      // ordinary, and neither is the app's problem to handle.
      this._closed = true;
      return false;
    }
  }

  /**
   * Gone: this was never a schedule.
   *
   * It held the task's *name*, so a page rendering "last scheduled run:
   * ${task.schedule}" published *last scheduled run: refresh*. Throwing rather
   * than aliasing `name` is the point — an app that wanted the cron expression
   * cannot have it from any SDK call, and an alias would keep that page wrong
   * under a name that reads right.
   */
  get schedule() {
    throw new Error(
      "Task.schedule held the task's name, not its cron expression, and is now " +
        "Task.name. No SDK call gives an app its own cron expression: the platform " +
        "sends the name so that one endpoint serving several schedules can tell " +
        "them apart.",
    );
  }

  toString() {
    return `[Task ${this.name} run=${this.id}]`;
  }
}

/**
 * Build a `Task` from the request headers, or `null`.
 *
 * `null` means this request is not a scheduled firing — somebody opened the
 * endpoint in a browser, a health check hit it, or the app is running outside
 * the gateway. That is not an error and must not throw: the same endpoint
 * serving both a schedule and a person is the ordinary shape of a report app.
 *
 * This mirrors `toft.viewer` in construction and differs in exactly that: a
 * missing act-for header throws, because a run sliding out of a person's name
 * into nobody's is a fact somebody has to notice; a missing cron header is just
 * a visitor.
 */
export function task(headers) {
  const runId = findHeader(headers, RUN_HEADER);
  if (!runId) return null;
  return new Task(runId, findHeader(headers, SCHEDULE_HEADER));
}

function findHeader(headers, name) {
  if (!headers) return "";
  const wanted = name.toLowerCase();

  if (typeof headers.get === "function") {
    const value = headers.get(name) ?? headers.get(wanted);
    if (value) return String(value);
  }
  if (typeof headers.entries === "function") {
    for (const [key, value] of headers.entries()) {
      if (String(key).toLowerCase() === wanted && value) return String(value);
    }
    return "";
  }
  for (const [key, value] of Object.entries(headers)) {
    if (key.toLowerCase() === wanted && value) return String(value);
  }
  return "";
}
