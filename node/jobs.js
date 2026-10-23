/**
 * Starting background work from inside an app.
 *
 * This is an app starting **a different entrypoint of itself**. Same repo, same
 * release, same storage, different command — `toft.toml` declares it, and
 * the platform runs it as an isolated workload with a memory limit, a deadline
 * and a durable record.
 *
 *   import toft from "@larchway/toft";
 *
 *   // Hono, Next.js route handlers, anything that returns a Fetch Response:
 *   app.post("/reports", async (c) => {
 *     const me = toft.viewer(c.req.raw.headers);           // the person clicking
 *     const run = await me.jobs.start("nightly-report", { params: { region: "emea" } });
 *     return toft.jobs.respond(run, c.req.raw);            // 202, or 303 to its status page
 *   });
 *
 *   // Express, which does not send a Fetch Response itself:
 *   const r = toft.jobs.respond(run, req);
 *   res.status(r.status).set(Object.fromEntries(r.headers)).send(await r.text());
 *
 * **Respond, don't wait.** A web request has a deadline — the gateway, and any
 * load balancer in front of it, cuts it off — and a job's cold start alone can
 * be tens of seconds. `respond(run, req)` answers now: `202 Accepted` with
 * `Location` and the run as JSON for a script, `303 See Other` for a browser
 * (its `Accept` names `text/html`), which lands on the run's live status page.
 * The gateway serves that page on the app's own origin; the app writes no
 * polling code and no template. `req` is a Fetch `Request` or a Node
 * `IncomingMessage`; without one the JSON answer is the one given.
 *
 * `run.wait()` is for scripts and for one job waiting on another. **Called
 * while serving a request it rejects** (`would_outlive_request`) unless its
 * `timeoutMs` fits inside what is left of that request — the gateway puts the
 * cut-off on every request it forwards, as `X-Toft-Request-Deadline`, and a
 * wait that would outlive it ends as a 504 nobody can explain. The SDK sees the
 * request two ways, both explicit: the headers given to `toft.viewer()` (every
 * run started or read through that viewer carries them), and a `headers` option
 * to `wait`. A WebSocket upgrade's headers carry no deadline, because its
 * deadline is not the socket's. **There is no third way**: Node has no current
 * request to find, so a run from `toft.jobs.start()` waited on with no
 * `headers` cannot tell a handler from a script, and waits to the 504.
 *
 * Which object you call says whether a run is attributed to a person —
 * `toft.jobs` is the app acting as itself, `toft.viewer(headers).jobs` the app
 * acting for the person looking at the page. A flag would make the anonymous
 * case the default spelling of the attributed one, and attribution would then
 * be lost by omission.
 *
 * **A repeated start is not a second run.** The platform derives an idempotency
 * key from the app, the job, the parameters and the resolved person; a start
 * matching a run already in flight, or one that finished in the last minute,
 * comes back as *that* run with `deduplicated` set. Reported, never silent.
 *
 * **Params are the only input.** They are typed by `toft.toml`, recorded on the
 * run and shown in its history, so they must not carry secrets and are capped
 * at 64 KB. Anything bigger goes into the app's own storage first, and the param
 * names where.
 *
 * **A run is never retried.** A failed run stays failed, with `failureReason`
 * saying why in words; trying again is a person's decision (`toft job rerun`),
 * which records the new run's `rerunOf`.
 */

import { ToftError, transport } from "./transport.js";
import { findHeader } from "./viewer.js";

const JOBS_PATH = "/api/v1/app/jobs";
const RUNS_PATH = "/api/v1/app/job-runs";

/**
 * When the request the app is serving will be cut off, RFC 3339. The gateway
 * sets it on every request it forwards; it is absent outside one.
 */
export const REQUEST_DEADLINE_HEADER = "X-Toft-Request-Deadline";

/** Statuses that will not change again. Polling stops here. */
export const TERMINAL_STATUSES = new Set([
  "succeeded",
  "failed",
  "timed_out",
  "out_of_memory",
  "cancelled",
  "skipped",
]);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// How long a polling loop sleeps after a rate_limited refusal: what
// Retry-After asked, else one poll interval — never past the wait's deadline.
function backoffMs(err, pollIntervalMs, deadline) {
  let pause = err.retryAfter !== undefined ? err.retryAfter * 1000 : pollIntervalMs;
  if (deadline !== undefined) pause = Math.min(pause, Math.max(deadline - Date.now(), 0));
  return pause;
}

/**
 * The cut-off of the request these headers came in on, as a Date, or null.
 *
 * Null for a WebSocket upgrade: an app holding its handshake headers for the
 * life of a socket would otherwise carry a deadline that passed hours ago and
 * never meant anything.
 */
export function requestDeadline(headers) {
  if (!headers) return null;
  if (findHeader(headers, "Upgrade").toLowerCase() === "websocket") return null;
  const raw = findHeader(headers, REQUEST_DEADLINE_HEADER);
  if (!raw) return null;
  const at = new Date(raw);
  return Number.isNaN(at.getTime()) ? null : at;
}

/**
 * This app already has as many runs queued as it is allowed.
 *
 * A first-class, actionable refusal carrying the numbers, so an app renders "3
 * runs already queued, and 3 is the limit" instead of putting a traceback in
 * the middle of somebody's page. `retryable` is true, because the platform
 * says a full queue drains.
 */
export class QueueFull extends ToftError {
  constructor(message, options = {}) {
    super(message, options);
    this.name = "QueueFull";
    this.inFlight = options.body?.in_flight ?? 0;
    this.limit = options.body?.limit ?? 0;
  }
}

/**
 * A run, and everything an app can do with one.
 *
 * Timestamps are the platform's RFC 3339 text, or null where the run has not
 * reached them.
 */
export class JobRun {
  constructor(raw, headers, { requestDeadline = null } = {}) {
    const data = raw ?? {};
    const text = (key) => (data[key] === undefined || data[key] === null || data[key] === "" ? null : String(data[key]));
    this.id = data.id ?? "";
    this.job = data.job ?? "";
    /** queued, running, succeeded, failed, timed_out, out_of_memory, cancelled or skipped. */
    this.status = data.status ?? "queued";
    /**
     * app, viewer, cli, schedule or ui. `viewer` never says *which* person: an
     * app does not learn that.
     */
    this.startedBy = data.started_by ?? "";
    /** The run this one re-ran, when a person asked for that. */
    this.rerunOf = text("rerun_of");
    this.params = data.params ?? {};
    this.scheduledFor = text("scheduled_for");
    this.queuedAt = text("queued_at");
    this.startedAt = text("started_at");
    this.finishedAt = text("finished_at");
    /** When the run will be stopped if it is still going. */
    this.deadline = text("deadline");
    this.progress = data.progress ?? 0;
    this.progressMessage = text("progress_message");
    /** 1-based while queued, null once the run starts. */
    this.queuePosition = data.queue_position || null;
    this.hasOutput = Boolean(data.has_output);
    this.exitCode = data.exit_code ?? null;
    this.failureStage = text("failure_stage");
    /** Why it did not succeed, in words: `exit 3: connection refused`, `timed out after 45m`. */
    this.failureReason = text("failure_reason");
    this.peakMemoryMb = data.peak_memory_mb ?? null;
    /** The run's live status page, on the app's own origin — where `respond` sends a browser. */
    this.statusUrl = data.status_url ?? "";
    this.logsUrl = data.logs_url ?? "";
    /** True when this start returned a run that already existed. */
    this.deduplicated = Boolean(data.deduplicated);
    this.raw = data;
    // Not enumerable: the act-for blob is a bearer credential for somebody's
    // session, and a console.log of a run is exactly where it should not land.
    Object.defineProperty(this, "_headers", { value: headers ?? {}, enumerable: false });
    Object.defineProperty(this, "_requestDeadline", { value: requestDeadline, enumerable: false });
  }

  /** The run as the platform sent it — what `respond` answers with. */
  toJSON() {
    return Object.keys(this.raw).length ? this.raw : { id: this.id, job: this.job, status: this.status };
  }

  /** Whether this run will change again. */
  get done() {
    return TERMINAL_STATUSES.has(this.status);
  }

  get succeeded() {
    return this.status === "succeeded";
  }

  /** Started in a person's name rather than by the app itself. */
  get startedByViewer() {
    return this.startedBy === "viewer";
  }

  /**
   * Stopped for exceeding its limits, rather than crashing. Worth telling apart
   * because the fix is different: a bigger limit or less work, not a code
   * change.
   */
  get outOfResources() {
    return this.status === "timed_out" || this.status === "out_of_memory";
  }

  #again(raw) {
    return new JobRun(raw, this._headers, { requestDeadline: this._requestDeadline });
  }

  /** Re-read this run. */
  async refresh() {
    return this.#again(await transport().get(`${RUNS_PATH}/${encodeURIComponent(this.id)}`, this._headers));
  }

  /**
   * Resolve with the run in its final state.
   *
   * For scripts and jobs. **On a request path it rejects**
   * (`would_outlive_request`) unless `timeoutMs` fits inside what is left of the
   * request — see the top of this file for how the request is seen; `headers` is
   * the explicit way to say "I am serving this one". The answer there is
   * `toft.jobs.respond(run, req)`.
   *
   * Rejects with `wait_timeout` (retryable) when `timeoutMs` passes first.
   */
  async wait({ timeoutMs, pollIntervalMs = 2000, headers } = {}) {
    this.#guardRequest(timeoutMs, headers);
    const deadline = timeoutMs === undefined ? undefined : Date.now() + timeoutMs;
    let current = this;
    for (;;) {
      try {
        current = await current.refresh();
      } catch (err) {
        // A poll the app's own rate limit refused is not the run failing:
        // wait out Retry-After and read again. Every other app call spends the
        // same limiter, so a busy app hits it mid-wait.
        if (err?.code !== "rate_limited") throw err;
        if (deadline !== undefined && Date.now() >= deadline) {
          throw new ToftError(
            `run ${this.id} was still ${current.status} after ${timeoutMs}ms`,
            { body: { code: "wait_timeout", retryable: true } },
          );
        }
        await sleep(backoffMs(err, pollIntervalMs, deadline));
        continue;
      }
      if (current.done) return current;
      if (deadline !== undefined && Date.now() >= deadline) {
        // Named, so a caller can branch on it: the run is still going, and
        // waiting again is a fair answer — hence retryable.
        throw new ToftError(
          `run ${this.id} was still ${current.status} after ${timeoutMs}ms`,
          { body: { code: "wait_timeout", retryable: true } },
        );
      }
      await sleep(pollIntervalMs);
    }
  }

  #guardRequest(timeoutMs, headers) {
    const cutoff = headers !== undefined ? requestDeadline(headers) : this._requestDeadline;
    if (!cutoff) return;
    const leftMs = cutoff.getTime() - Date.now();
    if (timeoutMs !== undefined && timeoutMs <= leftMs) return;
    const asked = timeoutMs === undefined ? "no timeout" : `a ${timeoutMs}ms timeout`;
    throw new ToftError(
      `wait() on run ${this.id} would outlive the request it is serving: the gateway cuts ` +
        `this request off in ${Math.max(Math.round(leftMs / 1000), 0)}s, and this wait has ` +
        `${asked}. Answer the request now with toft.jobs.respond(run, req) — a browser lands ` +
        `on the run's live status page — and let the run finish on its own.`,
      {
        body: {
          code: "would_outlive_request",
          retryable: false,
          hint: "return toft.jobs.respond(run)",
        },
      },
    );
  }

  /**
   * Fetch this run's output object, or an empty buffer if it produced none.
   *
   * Over a presigned URL the platform mints — the app holds no object-store
   * credential, because one that could read this run's output could read every
   * other app's data.
   *
   * Rejects when the artifact *did* exist and has since expired. An output
   * object lives exactly as long as the run row that explains it, and saying so
   * beats a download that quietly 404s: a run still reading `succeeded` with
   * nothing behind it is the worst failure shape available here.
   */
  async output() {
    return transport().follow(`${RUNS_PATH}/${encodeURIComponent(this.id)}/output`, this._headers);
  }

  /**
   * The output `writeOutputJson` wrote, decoded — `null` when the run produced
   * none. The reading twin every caller otherwise writes by hand.
   */
  async outputJson() {
    const raw = await this.output();
    return raw && raw.length ? JSON.parse(Buffer.from(raw).toString("utf8")) : null;
  }

  /** Log lines after `afterSeq`: `{seq, at, stream, line}` each. */
  async logs(afterSeq = 0) {
    const raw = await transport().get(
      `${RUNS_PATH}/${encodeURIComponent(this.id)}/logs?after=${Number(afterSeq)}`,
      this._headers,
    );
    return raw?.lines ?? [];
  }

  /** Async-iterate log lines until the run reaches a terminal status. */
  async *tail({ pollIntervalMs = 2000 } = {}) {
    let seq = 0;
    for (;;) {
      let lines, done;
      try {
        lines = await this.logs(seq);
        done = (await this.refresh()).done;
      } catch (err) {
        if (err?.code !== "rate_limited") throw err;
        await sleep(backoffMs(err, pollIntervalMs));
        continue;
      }
      for (const line of lines) {
        seq = Math.max(seq, line.seq ?? seq);
        yield line;
      }
      if (done) {
        // One last read: lines can land between the poll and the status check,
        // and losing the end of a failing job's output loses the part that
        // explains it.
        for (const line of await this.logs(seq)) yield line;
        return;
      }
      await sleep(pollIntervalMs);
    }
  }

  /**
   * Stop this run.
   *
   * A viewer may cancel a run they started and not somebody else's; the app
   * acting as itself may cancel a run with no person behind it. Cancelling
   * through the wrong one of those rejects rather than quietly doing nothing.
   */
  async cancel() {
    await transport().postJson(`${RUNS_PATH}/${encodeURIComponent(this.id)}/cancel`, undefined, {
      headers: this._headers,
    });
  }
}

/**
 * Answer the request that started `run`, without waiting for it: a Fetch
 * `Response`. A browser (`Accept` naming `text/html`) gets `303 See Other` to
 * the run's live status page; anything else `202 Accepted` with `Location` and
 * the run as JSON. `req` is a Fetch `Request`, a Node `IncomingMessage`, or
 * just their headers.
 */
export function respond(run, req) {
  const headers = req?.headers ?? req;
  const html = findHeader(headers, "Accept").toLowerCase().includes("text/html");
  const location = run.statusUrl ? { Location: run.statusUrl } : {};
  if (html) return new Response(null, { status: 303, headers: location });
  return new Response(JSON.stringify(run), {
    status: 202,
    headers: { ...location, "Content-Type": "application/json" },
  });
}

/**
 * The job surface, either as the app or on a person's behalf.
 *
 * `toft.jobs` is the app acting as itself. `toft.viewer(headers).jobs` is the same
 * surface with a viewer attached, and the difference is one relayed header —
 * which the app cannot mint, alter, or read.
 */
export class Jobs {
  constructor(headers, { requestDeadline = null } = {}) {
    Object.defineProperty(this, "_headers", { value: headers ?? {}, enumerable: false });
    this._requestDeadline = requestDeadline;
  }

  #run(raw) {
    return new JobRun(raw, this._headers, { requestDeadline: this._requestDeadline });
  }

  /**
   * Queue a run and resolve with its handle immediately.
   *
   * `params` are checked against the job's `[jobs.<name>.params]` — a missing,
   * mistyped, out-of-choice or undeclared one is refused here (`param_required`,
   * `param_type`, `param_choice`, `param_unknown`), not an hour into the run. They are recorded
   * and shown, so they must not carry secrets.
   *
   * `idempotencyKey` overrides the derived one (and holds for 24 hours rather
   * than a minute). The derivation is already right for the case that actually
   * bites, so pass one only when the app has a better notion of "the same
   * request" than the parameters give.
   */
  async start(job, { params, idempotencyKey, ...rest } = {}) {
    if (Object.keys(rest).length) {
      // An option this version does not take — `payload` most likely — is
      // refused rather than dropped: a run started without the input its
      // caller meant to give it fails somewhere much harder to read.
      throw new ToftError(
        `jobs.start does not take ${Object.keys(rest).join(", ")}: params are a run's only ` +
          `input, and anything bigger belongs in the app's storage, named by a param`,
        { body: { code: "bad_request", retryable: false } },
      );
    }
    const body = {};
    if (params) body.params = params;
    try {
      const raw = await transport().postJson(`${JOBS_PATH}/${encodeURIComponent(job)}/runs`, body, {
        idempotencyKey,
        headers: this._headers,
      });
      return this.#run(raw);
    } catch (err) {
      throw typed(err);
    }
  }

  /** `respond(run, req)`, reached from the surface that started the run. */
  respond(run, req) {
    return respond(run, req);
  }

  async get(runId) {
    return this.#run(await transport().get(`${RUNS_PATH}/${encodeURIComponent(runId)}`, this._headers));
  }

  /** Every run of this app, newest first — of one job, in one status, when given. */
  async list({ job, status, limit = 50 } = {}) {
    return this.#list("app", { job, status, limit });
  }

  /**
   * This viewer's runs of this app, newest first.
   *
   * Only on a `toft.viewer` — the app acting as itself has no "mine" to ask
   * about, and calling it there rejects rather than quietly returning
   * everything.
   *
   * This is what makes coming back tomorrow work without the app storing a
   * single per-user row: the run is a durable record on the platform keyed on
   * the *person*, so they can start it on a laptop and check it from a phone.
   * Authorization stays live too — lose read on the app overnight and the list
   * is empty, because the filter runs against a principal the platform resolved
   * itself.
   */
  async mine(options = {}) {
    if (typeof options !== "object" || options === null) {
      // The old `mine(job, { limit })`: destructuring a string would drop the
      // job filter and answer with every job's runs, which reads as right.
      throw new ToftError(
        `mine() takes one options object — mine({ job: ${JSON.stringify(options)}, limit })`,
        { body: { code: "bad_request", retryable: false, hint: "mine({ job, limit })" } },
      );
    }
    const { job, limit = 20 } = options;
    if (!Object.keys(this._headers).length) {
      throw new ToftError(
        "mine() needs a viewer — call it on toft.viewer(req.headers).jobs. The app acting " +
          "as itself has no viewer to scope to, and answering with every run would be a " +
          "silent widening rather than an answer.",
        { body: { code: "no_viewer", retryable: false, hint: "call it on toft.viewer(req.headers).jobs" } },
      );
    }
    return this.#list("viewer", { job, limit });
  }

  async #list(scope, { job, status, limit }) {
    let query = `?scope=${scope}&limit=${Number(limit)}`;
    if (job) query += `&job=${encodeURIComponent(job)}`;
    if (status) query += `&status=${encodeURIComponent(status)}`;
    const raw = await transport().get(`${RUNS_PATH}${query}`, this._headers);
    return (raw?.runs ?? []).map((r) => this.#run(r));
  }
}

/**
 * Promote a queue-full refusal to the error a caller can render. Flattening the
 * counts into "429 Too Many Requests" throws away the only part an app could
 * put on a page.
 */
function typed(err) {
  if (err instanceof ToftError && err.status === 429 && err.code === "queue_full") {
    return new QueueFull(err.message, {
      status: err.status,
      retryAfter: err.retryAfter,
      body: err.body,
    });
  }
  return err;
}
