/**
 * The Toft app SDK for Node.
 *
 * One import, one entry point:
 *
 *   import toft from "@larchway/toft";
 *
 *   await toft.app.slug();                  // who am I
 *   await toft.app.can("email");            // what did an administrator attach
 *
 *   toft.scratchDir("uploads");             // ephemeral, per app, no network
 *   toft.sessionScratchDir(sessionId);      // one browser login's corner of it
 *   toft.newSessionId();                    // …for a viewer who signed in nowhere
 *   toft.storage("reports").path;           // durable, resolved by name
 *   toft.storage("drop", {folder:"backup"}); // …and by folder, where one
 *                                        // resource is mapped twice
 *   await toft.store("reports").put(file);  // a file into a store
 *
 *   await toft.data.get();                  // this app's one JSON document
 *   await toft.data.set({ period });        // survives restart, redeploy, rollback
 *   await toft.data.update((s) => { s.runs++; });   // …and a second writer
 *
 *   await toft.notify.slack("Ingest finished: 1,204 rows");
 *   await toft.notify.email({ to, subject, text });     // as this app, not as you
 *
 *   await toft.jobs.start("reconcile", { params });     // as the app
 *   const me = toft.viewer(req.headers);                // as a person
 *   await me.jobs.start("reconcile", { params });
 *   await me.jobs.mine("reconcile", { limit: 20 });
 *
 *   const who = await toft.who(req.headers);            // who is looking; null if nobody
 *   if (who?.role === "owner" || who?.isAdmin) renderAdminTab();
 *
 *   const c = toft.caller(req.headers);                 // what they may do; no round trip
 *   if (!c.atLeast(toft.ROLE_EDITOR)) return res.status(403).end();
 *
 *   toft.run                                // inside a job; null everywhere else
 *   const task = toft.task(req.headers);    // a scheduled firing; else null
 *   await task?.log("read 1,204 rows");   // a log the platform cannot capture
 *
 * **Background work is a different entrypoint of this same app.** `jobs.start`
 * runs a command `toft.toml` declares, from the same release, as an
 * isolated workload with a memory limit, a deadline and a durable record — in
 * *both* execution modes, because moving twelve minutes and six gigabytes out
 * of the app's own process is the point rather than a side effect. It never
 * blocks on the work.
 *
 * **Two objects, and which one you call is the decision.** `toft.jobs` is the app
 * acting as itself; `toft.viewer(headers).jobs` is the app acting for the person
 * looking at the page. Not a flag, because a flag would make the anonymous case
 * the default spelling of the attributed one, and attribution would then be
 * lost by omission.
 *
 * **Two entrypoints for a viewer, and which one throws is the difference.**
 * `toft.viewer(headers)` is *act as this person* and throws when there is nobody
 * to act for. `toft.who(headers)` is *who is this*, and resolves to `null` for an
 * anonymous visitor because that is an answer rather than a failure. It throws
 * for one thing only: an app an administrator has not enabled viewer identity
 * on, which is a configuration gap to close and not a runtime state to branch
 * on.
 *
 * **A scheduled task's log is the one thing the platform cannot capture for
 * you.** Toft triggers a task by making an HTTP request to a path you
 * published; it owns no process, so your prints land in the app's own stream
 * interleaved with every other request. `toft.task(req.headers)` is that firing —
 * `null` on an ordinary visit, so one endpoint can serve both a schedule and a
 * person — and `task.log(...)` is what makes a run's log exist. Worth adding to
 * anything that might outlive the timeout: a run that never answers is recorded
 * as a failure with nothing else in it.
 *
 * **A failed run may be retried, and you configure that in Toft, not
 * here.** Retries are off by default and switched on per task or job — on the
 * app's Automation tab, or as `retries = N` in `toft.toml`. What the SDK
 * does is keep the chain from being your problem: a run the platform has queued
 * another attempt at is not `done`, and `refresh()` follows it, so `wait()`
 * gives you the outcome of the work rather than of the first try at it. Only
 * runs nobody is waiting on are eligible — a schedule, and what the app started
 * — never one somebody kicked off from the Toft panel.
 *
 * **Off-platform it throws rather than pretending.** Every call that needs the
 * platform names the environment variable that is missing. There is deliberately
 * no dry-run mode. The scratch helpers are the one exception, because a path is
 * a path.
 *
 * `Launcher` is gone rather than deprecated. It could never work: it read
 * `TOFT_APP_ID`, which nothing set, and presented `TOFT_JOB_TOKEN` —
 * minted with the *run* audience — to a route that wanted a session or a
 * personal key, so the call authenticated as nobody and 404d. Enqueueing
 * arrives as `toft.jobs` rather than as a repair to that.
 *
 * No dependencies: this runs inside a customer's app image beside their own
 * tree, and a version conflict with our HTTP library would be our bug showing
 * up in their build.
 */

import { createHash, randomBytes } from "node:crypto";
import { mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { App, Mount, mounts, storage, store } from "./app.js";
import {
  Caller,
  ENV_ROLE_KEY,
  ROLE_ANONYMOUS,
  ROLE_APP,
  ROLE_EDITOR,
  ROLE_HEADER,
  ROLE_OWNER,
  ROLE_SYSTEM,
  ROLE_UNKNOWN,
  ROLE_VIEWER,
  caller,
} from "./caller.js";
import { Data, Document, VersionConflict } from "./data.js";
import { JobRun, Jobs, QueueFull, TERMINAL_STATUSES } from "./jobs.js";
import { Notify, Send } from "./notify.js";
import {
  APP_CONTRACT_VERSION,
  AppTransport,
  ToftError,
  errorFromText,
  resetTransport,
  transport,
} from "./transport.js";
import { CRON_HEADER, RUN_HEADER, SCHEDULE_HEADER, Task, task } from "./task.js";
import {
  ACT_FOR_HEADER,
  IDENTITY_TTL_MS,
  Viewer,
  resetIdentityCache,
  viewer,
  who,
} from "./viewer.js";

/**
 * The runner callback contract version, spoken by `Run` below. It must equal
 * the platform's `runner.ContractVersion`: a partial upgrade is reported rather
 * than tolerated, so a stale number here is a 409 on every callback a job makes.
 * `sdk/contract_test.go` keeps the four copies of it in step.
 */
const CONTRACT_VERSION = "2";
const DEFAULT_TIMEOUT_MS = 30_000;

/**
 * The runner-contract request helper, used only by `Run`.
 *
 * Deliberately separate from the app transport: a job reports over
 * `/api/v1/runner/*` with a run-scoped token, which is a different credential
 * with a different audience from the app token. Sharing one client between them
 * would be the first step towards sharing a credential.
 */
async function runnerRequest(baseUrl, token, method, path, body, timeoutMs = DEFAULT_TIMEOUT_MS) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const resp = await fetch(`${baseUrl.replace(/\/$/, "")}${path}`, {
      method,
      signal: controller.signal,
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json",
        "X-Toft-Runner-Version": CONTRACT_VERSION,
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });

    const text = await resp.text();
    // The transport's own reading of a refusal, so a job's run calls carry the
    // envelope — code, retryable, hint — exactly as the app's calls do.
    if (!resp.ok) throw errorFromText(resp, text);
    return text ? JSON.parse(text) : null;
  } catch (err) {
    if (err instanceof ToftError) throw err;
    if (err.name === "AbortError") {
      throw new ToftError(`Toft did not answer within ${timeoutMs}ms`);
    }
    throw new ToftError(`cannot reach Toft: ${err.message}`);
  } finally {
    clearTimeout(timer);
  }
}

/**
 * What a failed input read means, in the two cases that are not "it broke".
 *
 * Written out because the failures are indistinguishable at the HTTP layer and
 * the fixes are opposite: one is a job asking for something it was never given,
 * the other is a job that took too long. The same two sentences the Python SDK
 * raises.
 */
function inputRefusal(status) {
  if (status === 404) {
    return (
      "this run was given no payload — TOFT_INPUT_URI points at an object that was " +
      "never written. A run started with parameters alone has no input; read run.params " +
      "for those. (A current platform leaves run.inputUri empty in that case, so an " +
      "install still exporting it is older than this SDK.)"
    );
  }
  if (status === 403 || status === 410) {
    return (
      "this run's payload is no longer readable: the presigned link the platform minted " +
      "for it has expired. Read the input at the start of a run rather than hours into one."
    );
  }
  return `reading this run's input failed: HTTP ${status}`;
}

/** The job's own view of itself. */
export class Run {
  constructor({ baseUrl, token, id } = {}) {
    this._baseUrl = baseUrl ?? process.env.TOFT_API_URL;
    this._token = token ?? process.env.TOFT_JOB_TOKEN;
    this.id = id ?? process.env.TOFT_RUN_ID;
    this.inputUri = process.env.TOFT_INPUT_URI ?? "";
    this.outputUri = process.env.TOFT_OUTPUT_URI ?? "";
    /**
     * `trigger` is *how* this run started: "cron", "manual", "api" or "app".
     * `triggerRole` is *who* — one of the ROLE_* words, `"system"` for a
     * schedule firing on its schedule, `"app"` for the app starting work for
     * itself, and `"unknown"` where the platform did not say.
     *
     * The pair is the point. "manual" tells a report job nothing about whether
     * to include the private columns; "manual" plus "viewer" does. Both have
     * been recorded on every run since jobs existed and neither had a name in
     * this SDK until now.
     *
     * **The role is today's, not the day the run was queued.** It is derived
     * from the run's actor when the workload starts, so somebody demoted in
     * between reports their current role — the right answer for a job deciding
     * what work to do, and the wrong one for an audit. The audit question is
     * answered by the run's actor in Toft's own history, which does not
     * move.
     */
    this.trigger = process.env.TOFT_TRIGGER ?? "";
    this.triggerRole = process.env.TOFT_TRIGGER_ROLE || ROLE_UNKNOWN;

    if (!this._baseUrl || !this._token || !this.id) {
      throw new ToftError(
        "not running inside a Toft job — TOFT_API_URL, TOFT_JOB_TOKEN " +
          "and TOFT_RUN_ID are set by the runner",
      );
    }
  }

  static fromEnv() {
    return new Run();
  }

  get params() {
    const raw = process.env.TOFT_PARAMS;
    return raw ? JSON.parse(raw) : {};
  }

  /**
   * The instant this run is for, or `null` when nobody gave one. A scheduled
   * run's is the tick it fired for, in the schedule's own zone; a run started
   * with `toft job run --for` carries what was named. Never defaulted to now — a
   * re-run of Tuesday's work on Thursday must not quietly process Thursday.
   * `scheduledForText` keeps the zone offset, which a Date cannot.
   */
  get scheduledFor() {
    const raw = process.env.TOFT_SCHEDULED_FOR;
    return raw ? new Date(raw) : null;
  }

  get scheduledForText() {
    return process.env.TOFT_SCHEDULED_FOR || null;
  }

  /** Report how far along this run is, 0–100. */
  async progress(percent) {
    await runnerRequest(this._baseUrl, this._token, "POST", "/api/v1/runner/progress", {
      version: CONTRACT_VERSION,
      percent: Math.round(percent),
    });
  }

  /** Record the run's structured result. */
  async setResult(result) {
    await runnerRequest(this._baseUrl, this._token, "POST", "/api/v1/runner/result", {
      version: CONTRACT_VERSION,
      result,
      exit_code: 0,
      // The run is still going; only the runner's own report at exit ends it.
      record_only: true,
    });
  }

  /**
   * Fetch the payload this run was given, over a presigned URL — a job holds
   * no object-store credential, because one that could read this run's input
   * could read every other app's.
   */
  /**
   * Fetch the payload this run was given, or `null` when it was given none.
   *
   * **A refusal here is named rather than relayed.** The platform mints this URL
   * only for a run that was actually given a payload, so a 404 means an install
   * that predates that rule; a 403 or 410 is the link's six-hour window having
   * closed under a very long run. Both used to surface as `reading input: Not
   * Found` — a failure an author could not tell from a store that was down.
   */
  async readInput() {
    if (!this.inputUri) return null;
    const resp = await fetch(this.inputUri);
    if (!resp.ok) throw new ToftError(inputRefusal(resp.status), { status: resp.status });
    return resp.arrayBuffer();
  }

  async readInputJson() {
    const buf = await this.readInput();
    return buf && buf.byteLength ? JSON.parse(new TextDecoder().decode(buf)) : null;
  }

  /**
   * Upload this run's output. Also a presigned URL, and also in both
   * execution modes: a filesystem path would work in shared mode and would
   * then make an app written against it unable to move to isolated mode.
   */
  async writeOutput(data) {
    if (!this.outputUri) throw new ToftError("this run has no output location");
    const resp = await fetch(this.outputUri, { method: "PUT", body: data });
    if (!resp.ok) throw new ToftError(`uploading output: ${resp.statusText}`, { status: resp.status });
  }

  async writeOutputJson(value) {
    await this.writeOutput(JSON.stringify(value));
  }
}

/**
 * The request header carrying the viewer's opaque per-login session id, set
 * by the Toft proxy. Express lowercases header names in `req.headers`;
 * `req.get(SESSION_HEADER)` matches case-insensitively.
 */
export const SESSION_HEADER = "X-Toft-Session";

/**
 * The app's scratch directory, created if missing; optional `parts` name a
 * subpath, created too.
 *
 * Toft wipes it on every start, restart, and redeploy — it is per-app,
 * never backed up, and never shared with another app. The right home for
 * uploads being processed and intermediate results; durable data belongs on
 * a mapped storage resource (`TOFT_STORAGE`), not here. Outside
 * Toft the system temp directory stands in, so app code runs unchanged
 * in local development.
 */
export function scratchDir(...parts) {
  const root = process.env.TOFT_SCRATCH_DIR || tmpdir();
  const path = join(root, ...parts);
  mkdirSync(path, { recursive: true });
  return path;
}

/**
 * A per-session directory under `scratchDir()`, created if missing.
 *
 * `sessionId` is whatever the app read from the `X-Toft-Session` header
 * — an opaque id per browser login, absent on anonymous and API-key requests
 * — or any string of the app's own choosing, such as a framework's finer
 * per-connection session id. It is hashed before it becomes a directory
 * name, so a malformed or hostile value cannot traverse outside the scratch
 * root.
 *
 * Frameworks with their own session machinery (Streamlit, Shiny) need none
 * of this; hand-rolled Express/Fastify apps do. The per-session split is a
 * convention inside one app process, not a security boundary: all sessions
 * run under one OS identity, and the app's own code can list every session's
 * directory.
 *
 * Anonymous viewers carry no header, so the app mints its own id with
 * `newSessionId()` and carries it in a cookie of its own — header first, then
 * that cookie, then a fresh mint:
 *
 *   const VIEWER_SID = "viewer_sid";   // the app's own cookie; never a platform name
 *
 *   app.post("/upload", (req, res) => {
 *     const sid = req.headers["x-toft-session"] ||
 *       req.cookies[VIEWER_SID] || toft.newSessionId();
 *     const workdir = toft.sessionScratchDir(sid);
 *     res.cookie(VIEWER_SID, sid, { httpOnly: true, sameSite: "lax", secure: true });
 *     …
 *   });
 *
 * Substituting a constant for the missing header is the one thing this must
 * not do: it puts every anonymous viewer of a public app in the same
 * directory, where one viewer's upload overwrites another's.
 */
export function sessionScratchDir(sessionId, ...parts) {
  if (!sessionId) {
    throw new Error(
      "sessionScratchDir needs a non-empty session id — an empty one would " +
        "silently collapse every session into the same directory",
    );
  }
  const encoded = createHash("sha256").update(String(sessionId)).digest("hex").slice(0, 32);
  return scratchDir(encoded, ...parts);
}

/**
 * A fresh opaque session id, for a viewer the platform cannot name.
 *
 * Anonymous and API-key requests carry no `X-Toft-Session`, because there
 * is no server-side session to derive one from. An app that still wants
 * per-viewer scratch mints one here and carries it in a cookie of its own,
 * checked after the header and before this.
 *
 * Unguessable on purpose. The directory name is a hash of this string, so
 * anyone who can guess an id can name another viewer's directory — which is why
 * this is `randomBytes` and 128 bits rather than a counter or a timestamp.
 *
 * Two rules for the cookie that carries it:
 *
 *   - **Never a name under the `toft_` prefix** — that prefix is the
 *     platform's, and every cookie under it is stripped from a request before
 *     the app sees it, so an id stored there never survives a round trip and
 *     every request would silently mint another. Every other name, `session`
 *     included, is the app's own.
 *   - `HttpOnly`, `SameSite=Lax`, and `Secure` wherever the app is served over
 *     https.
 */
export function newSessionId() {
  return randomBytes(16).toString("base64url");
}


// --- The singletons app code reaches for ---
//
// All lazy: constructing them touches nothing, so importing this package
// off-platform never throws and never makes a network call.

/** This app, as the platform sees it. */
export const app = new App();

/**
 * This app's one JSON document: settings and small state that outlive a
 * restart, a redeploy and a rollback, and belong to nobody's session. Needs
 * nothing switched on — see data.js.
 */
export const data = new Data();

/** The job surface, with the app acting as itself. */
export const jobs = new Jobs();

/**
 * Slack and email sent *as this app*. The app supplies the message; the
 * platform supplies the identity — see notify.js.
 */
export const notify = new Notify();

/**
 * The job's own view of itself when this process *is* a job run, and `null`
 * everywhere else.
 *
 * `null` rather than throwing, because `if (toft.run)` is how a module shared
 * between an app's web entrypoint and its job entrypoint tells which one it is
 * in — and that is exactly the shape this feature encourages: one deployable,
 * two entrypoints.
 */
export const run = process.env.TOFT_RUN_ID ? new Run() : null;

export {
  ACT_FOR_HEADER,
  APP_CONTRACT_VERSION,
  ENV_ROLE_KEY,
  IDENTITY_TTL_MS,
  ROLE_ANONYMOUS,
  ROLE_APP,
  ROLE_EDITOR,
  ROLE_HEADER,
  ROLE_OWNER,
  ROLE_SYSTEM,
  ROLE_UNKNOWN,
  ROLE_VIEWER,
  AppTransport,
  CONTRACT_VERSION,
  Caller,
  caller,
  Data,
  Document,
  JobRun,
  Jobs,
  ToftError,
  Mount,
  Notify,
  QueueFull,
  Send,
  TERMINAL_STATUSES,
  Task,
  VersionConflict,
  Viewer,
  mounts,
  resetIdentityCache,
  resetTransport,
  storage,
  store,
  task,
  transport,
  viewer,
  who,
};

export default {
  app,
  data,
  jobs,
  notify,
  run,
  viewer,
  who,
  caller,
  task,
  storage,
  store,
  mounts,
  scratchDir,
  sessionScratchDir,
  newSessionId,
  SESSION_HEADER,
  ACT_FOR_HEADER,
  ROLE_HEADER,
  ROLE_UNKNOWN,
  ROLE_ANONYMOUS,
  ROLE_VIEWER,
  ROLE_EDITOR,
  ROLE_OWNER,
  ROLE_SYSTEM,
  ROLE_APP,
  CRON_HEADER,
  RUN_HEADER,
  SCHEDULE_HEADER,
  ToftError,
  QueueFull,
  VersionConflict,
};

