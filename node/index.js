/**
 * The Toft app SDK for Node.
 *
 * One import, one entry point:
 *
 *   import toft from "@larchway/toft";
 *
 *   toft.app.slug;                          // who am I — plain facts are properties
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
 *   const run = await me.jobs.start("reconcile", { params });
 *   return toft.jobs.respond(run, req);                 // 202, or 303 to its status page
 *   await me.jobs.mine({ job: "reconcile", limit: 20 });
 *
 *   const who = await toft.who(req.headers);            // who is looking; null if nobody
 *   if (who?.role === "owner" || who?.isAdmin) renderAdminTab();
 *
 *   const c = toft.caller(req.headers);                 // what they may do; no round trip
 *   if (!c.atLeast(toft.ROLE_EDITOR)) return res.status(403).end();
 *
 *   toft.run                                // inside a job; null everywhere else
 *
 * **Background work is a different entrypoint of this same app.** `jobs.start`
 * runs a command `toft.toml` declares, from the same release, as an
 * isolated workload with a memory limit, a deadline and a durable record — in
 * *both* execution modes, because moving twelve minutes and six gigabytes out
 * of the app's own process is the point rather than a side effect. It never
 * blocks on the work, and a request handler answers with
 * `toft.jobs.respond(run, req)` rather than waiting: `wait()` is for scripts
 * and jobs, and rejects inside a request it would outlive once it is shown
 * that request (`toft.viewer(headers)`, or `wait({ headers })`). Params are the only
 * input, and a run is never retried behind anyone's back. See jobs.js.
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
 * for one thing only: a request carrying a signed-in viewer to an app an
 * administrator has not enabled viewer identity on, which is a configuration
 * gap to close and not a runtime state to branch on. A request with nobody on
 * it (anonymous, or an API key) is `null` whatever the setting;
 * `toft.app.can("viewer.identity")` reads the setting itself.
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
import {
  JobRun,
  Jobs,
  QueueFull,
  REQUEST_DEADLINE_HEADER,
  TERMINAL_STATUSES,
  respond,
} from "./jobs.js";
import { Notify, Send } from "./notify.js";
import { Token, accessToken, azureCredential, resetTokenCache } from "./tokens.js";
import {
  APP_CONTRACT_VERSION,
  AppTransport,
  ToftError,
  errorFromText,
  resetTransport,
  transport,
} from "./transport.js";
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
 * The job's own view of itself: who started it, its params, its deadline;
 * report progress, log, write output.
 *
 *   const r = toft.run;                         // null outside a job
 *   const region = r.params.region;             // typed by toml: ints are numbers
 *   await r.progress(40, "reading ledgers");    // the status page shows both
 *   r.log("12,044 rows");                       // stdout; the runner ships it
 *   await r.writeOutputJson({ rows: 12044 });   // presigned PUT, both modes
 *
 * **Params are the only input.** There is no payload: anything bigger than a
 * param belongs in the app's own storage, and the param names where.
 * `writeOutput` is the one durable result channel, and it outlives scratch.
 */
export class Run {
  constructor({ baseUrl, token, id } = {}) {
    this._baseUrl = baseUrl ?? process.env.TOFT_API_URL;
    this._token = token ?? process.env.TOFT_JOB_TOKEN;
    this.id = id ?? process.env.TOFT_RUN_ID;
    /** The job this run is of — its `[jobs.<name>]` in `toft.toml`. */
    this.job = process.env.TOFT_JOB_NAME ?? "";
    /**
     * The run this one re-ran, or null. A rerun carries the same params; a job
     * that must not repeat a side effect checks this.
     */
    this.rerunOf = process.env.TOFT_RERUN_OF || null;
    this.outputUri = process.env.TOFT_OUTPUT_URI ?? "";
    /**
     * `trigger` is *how* this run started — `app`, `viewer`, `cli`, `schedule`
     * or `ui`, the run's `startedBy`. `triggerRole` is *who* — one of the ROLE_*
     * words, `"system"` for a schedule firing on its schedule, `"app"` for the
     * app starting work for itself, and `"unknown"` where the platform did not
     * say.
     *
     * The pair is the point. "ui" tells a report job nothing about whether to
     * include the private columns; "ui" plus "viewer" does.
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

  /** The contract's name for `trigger`. */
  get startedBy() {
    return this.trigger;
  }

  /** The run's params, typed as `toft.toml` declares them. */
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

  /**
   * When the platform stops this run, or `null` where it did not say. A job
   * with a long loop checks it and stops cleanly, rather than being stopped
   * mid-write.
   */
  get deadline() {
    const raw = process.env.TOFT_DEADLINE;
    return raw ? new Date(raw) : null;
  }

  /**
   * Report how far along this run is, 0–100, and optionally what it is doing —
   * a few words the status page shows under the bar.
   */
  async progress(percent, message) {
    const body = { version: CONTRACT_VERSION, percent: Math.round(percent) };
    if (message) body.message = String(message);
    await runnerRequest(this._baseUrl, this._token, "POST", "/api/v1/runner/progress", body);
  }

  /**
   * Write one line to this run's log. Nothing but a write to stdout (stderr
   * with `{ error: true }`) — the runner ships both — and here so a job does
   * not have to know that.
   */
  log(message, { error = false } = {}) {
    (error ? process.stderr : process.stdout).write(`${message}\n`);
  }

  /**
   * Upload this run's output. Also a presigned URL, and also in both
   * execution modes: a filesystem path would work in shared mode and would
   * then make an app written against it unable to move to isolated mode.
   */
  async writeOutput(data) {
    if (!this.outputUri) {
      throw new ToftError("this run has no output location", { body: { code: "no_output", retryable: false } });
    }
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
    // Coded like every other refusal the SDK makes before a request, which is
    // what the skill promises; a bare Error carried no code.
    throw new ToftError(
      "sessionScratchDir needs a non-empty session id — an empty one would " +
        "silently collapse every session into the same directory",
      { body: { code: "bad_request", retryable: false } },
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
  REQUEST_DEADLINE_HEADER,
  Send,
  TERMINAL_STATUSES,
  Token,
  VersionConflict,
  accessToken,
  azureCredential,
  resetTokenCache,
  Viewer,
  mounts,
  resetIdentityCache,
  respond,
  resetTransport,
  storage,
  store,
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
  storage,
  store,
  mounts,
  scratchDir,
  sessionScratchDir,
  newSessionId,
  accessToken,
  azureCredential,
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
  ToftError,
  QueueFull,
  VersionConflict,
};

