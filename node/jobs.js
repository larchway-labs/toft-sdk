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
 *   // The app acting as itself — no viewer, no actor.
 *   const run = await toft.jobs.start("reconcile", { params: { month } });
 *
 *   // The app acting for whoever is looking at the page.
 *   const me = toft.viewer(req.headers);
 *   const run2 = await me.jobs.start("reconcile", { params: { month } });
 *   const recent = await me.jobs.mine("reconcile", { limit: 20 });
 *
 * Which object you call is what says whether a run is attributed to a person,
 * and that is deliberate: a flag would make the anonymous case the default
 * spelling of the attributed one, and attribution would then be lost by
 * omission.
 *
 * **Nothing here blocks on the work.** `start` resolves as soon as the run is
 * queued. `wait` exists, is for scripts and one-off tooling, and is never what
 * `start` does — an isolated workload's cold start alone is tens of seconds, so
 * a call that could resolve with a result would be one that sometimes holds a
 * request for a minute.
 *
 * **A repeated start is not a second run.** The platform derives an idempotency
 * key from the app, the job, the parameters and the resolved person; a start
 * matching a run already in flight, or one that finished in the last minute,
 * comes back as *that* run with `deduplicated` set. Reported, never silent.
 *
 * **A run you start may be retried, and this object follows the chain.** Retries
 * are off by default and switched on per job — on the app's Automation tab, or
 * as `retries = N` in `toft.toml` — never in code here. What the SDK does
 * is make sure the chain is not something you have to know about:
 *
 *   - `run.done` is **false** on a failed run the platform has already queued
 *     another attempt at. The work is still in progress, in a row with a
 *     different id, and reporting that as finished would announce a failure for
 *     something about to succeed.
 *   - `refresh()` returns the *newer attempt* when there is one, so `wait()`
 *     resolves on the outcome of the work rather than of the first try at it.
 *   - `run.attempt` is which try you are looking at, and `run.retriedBy` names
 *     the successor when one exists. Read them when the distinction matters;
 *     ignore them and the defaults are right.
 *
 * Which runs are eligible is the platform's rule, not this library's: a schedule
 * and anything the app started — including a run started on a viewer's behalf,
 * since the app fired it and did not block on it. A run somebody started from
 * the Toft panel is never retried behind them, and neither is one that ran
 * out of time or memory, because a second attempt would end the same way.
 */

import { ToftError, transport } from "./transport.js";

const JOBS_PATH = "/api/v1/app/jobs";
const RUNS_PATH = "/api/v1/app/job-runs";

/** Statuses that will not change again. Polling stops here. */
export const TERMINAL_STATUSES = new Set([
  "succeeded",
  "failed",
  "cancelled",
  "timed_out",
  "resource_exceeded",
  "infrastructure_error",
]);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * This app already has as many runs in flight as it is allowed.
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

/** A run, and everything an app can do with one. */
export class JobRun {
  constructor(raw, headers) {
    const data = raw ?? {};
    this.id = data.id ?? "";
    this.status = data.status ?? "pending";
    this.jobName = data.job_name ?? "";
    this.progress = data.progress ?? 0;
    /** 1-based while pending, undefined once the run starts. */
    this.queuePosition = data.queue_position || undefined;
    /** True when this start returned a run that already existed. */
    this.deduplicated = Boolean(data.deduplicated);
    /**
     * True when a person started this rather than the app itself. Never says
     * *which* person: an app does not learn that.
     */
    this.startedByViewer = Boolean(data.started_by_viewer);
    this.hasOutput = Boolean(data.has_output);
    this.exitCode = data.exit_code;
    this.failureStage = data.failure_stage ?? "";
    this.failureReason = data.failure_reason ?? "";
    /** The structured result the job recorded with `run.setResult`. */
    this.result = data.result;
    /**
     * Which try this run is, and the run that superseded it when the platform
     * queued another attempt. A failed run with `retriedBy` set is not the end
     * of the story — see `done` and `refresh`.
     */
    this.attempt = data.attempt ?? 1;
    this.retriedBy = data.retried_by ?? "";
    /** The instant this run is for — its schedule's tick, or one a person
     * named — as RFC 3339, or "" when nobody gave one. */
    this.scheduledFor = data.scheduled_for ?? "";
    this.raw = data;
    this._headers = headers ?? {};
  }

  /**
   * Whether this run will change again.
   *
   * A failed run the platform has already queued another attempt at is *not*
   * done: the work is still in progress, in a row with a different id. Reading
   * only this row's status there would report a failure for something that is
   * about to succeed.
   */
  get done() {
    return TERMINAL_STATUSES.has(this.status) && !this.retriedBy;
  }

  get succeeded() {
    return this.status === "succeeded";
  }

  /**
   * Killed for exceeding its limits, rather than crashing. Worth telling apart
   * because the fix is different: a bigger limit or less work, not a code
   * change.
   */
  get outOfResources() {
    return this.status === "resource_exceeded" || this.status === "timed_out";
  }

  /**
   * Re-read this run.
   *
   * **Follows a retry.** If the platform has queued another attempt at this
   * work, the handle you get back is that attempt — same chain, newer row. An
   * app that started a run and waits on it should see the outcome of the work,
   * not of the first try at it; `attempt` and `raw.retry_of` are there for
   * anyone who needs the distinction.
   */
  async refresh() {
    const run = new JobRun(
      await transport().get(`${RUNS_PATH}/${this.id}`, this._headers),
      this._headers,
    );
    if (!run.retriedBy) return run;
    return new JobRun(
      await transport().get(`${RUNS_PATH}/${run.retriedBy}`, this._headers),
      this._headers,
    );
  }

  /**
   * Resolve when the run finishes.
   *
   * For scripts and one-off tooling. **Never call this on a request path** —
   * that is the twelve-minute block this whole feature exists to remove.
   */
  async wait({ timeoutMs, pollIntervalMs = 2000 } = {}) {
    const deadline = timeoutMs === undefined ? undefined : Date.now() + timeoutMs;
    let current = this;
    for (;;) {
      current = await current.refresh();
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
    return transport().follow(`${RUNS_PATH}/${this.id}/output`, this._headers);
  }

  /**
   * The output `writeOutputJson` wrote, decoded — `null` when the run produced
   * none. The reading twin every caller otherwise writes by hand.
   */
  async outputJson() {
    const raw = await this.output();
    return raw && raw.length ? JSON.parse(Buffer.from(raw).toString("utf8")) : null;
  }

  async logs(afterSeq = 0) {
    const raw = await transport().get(
      `${RUNS_PATH}/${this.id}/logs?after=${afterSeq}`,
      this._headers,
    );
    return raw?.lines ?? [];
  }

  /** Async-iterate log lines until the run reaches a terminal status. */
  async *tail({ pollIntervalMs = 2000 } = {}) {
    let seq = 0;
    for (;;) {
      for (const line of await this.logs(seq)) {
        seq = Math.max(seq, line.seq ?? seq);
        yield line;
      }
      if ((await this.refresh()).done) {
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
    await transport().postJson(`${RUNS_PATH}/${this.id}/cancel`, undefined, {
      headers: this._headers,
    });
  }
}

/**
 * The job surface, either as the app or on a person's behalf.
 *
 * `toft.jobs` is the app acting as itself. `toft.viewer(headers).jobs` is the same
 * surface with a viewer attached, and the difference is one relayed header —
 * which the app cannot mint, alter, or read.
 */
export class Jobs {
  constructor(headers) {
    this._headers = headers ?? {};
  }

  /**
   * Queue a run and resolve with its handle immediately.
   *
   * `params` are recorded against the run for reproducibility and audit, so
   * they must not carry secrets — those belong in the app's managed
   * environment, which a run references rather than copies.
   *
   * `payload` is bulk input, stored in the object store and handed to the run
   * as a presigned URL; the job reads it with `run.readInputJson()`.
   *
   * `idempotencyKey` overrides the derived one. The derivation is already
   * correct for the case that actually bites, so pass one only when the app has
   * a better notion of "the same request" than the parameters give.
   */
  async start(job, { params, payload, idempotencyKey } = {}) {
    const body = {};
    if (params) body.params = params;
    if (payload !== undefined) body.payload = payload;

    try {
      const raw = await transport().postJson(`${JOBS_PATH}/${job}/runs`, body, {
        idempotencyKey,
        headers: this._headers,
      });
      return new JobRun(raw, this._headers);
    } catch (err) {
      throw typed(err);
    }
  }

  async get(runId) {
    return new JobRun(await transport().get(`${RUNS_PATH}/${runId}`, this._headers), this._headers);
  }

  /** Every run of this app, newest first. */
  async list({ job, limit = 50 } = {}) {
    return this.#list("app", job, limit);
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
  async mine(job, { limit = 20 } = {}) {
    if (!Object.keys(this._headers).length) {
      throw new ToftError(
        "mine() needs a viewer — call it on toft.viewer(req.headers).jobs. The app acting " +
          "as itself has no viewer to scope to, and answering with every run would be a " +
          "silent widening rather than an answer.",
        { body: { code: "viewer_required", retryable: false } },
      );
    }
    return this.#list("viewer", job, limit);
  }

  async #list(scope, job, limit) {
    let query = `?scope=${scope}&limit=${Number(limit)}`;
    if (job) query += `&job=${encodeURIComponent(job)}`;
    const raw = await transport().get(`${RUNS_PATH}${query}`, this._headers);
    return (raw?.runs ?? []).map((r) => new JobRun(r, this._headers));
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
