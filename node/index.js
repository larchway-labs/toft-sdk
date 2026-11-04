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
import { lstatSync, mkdirSync } from "node:fs";
import { tmpdir, userInfo } from "node:os";
import { basename, isAbsolute, join, relative, resolve, sep } from "node:path";
import { inspect } from "node:util";

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
  RUN_STATUSES,
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
  fetchStore,
  noMoreArgs,
  refusal,
  refuseRedirect,
  storeRefusal,
  resetTransport,
  takeOptions,
  toJson,
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
  const json = body === undefined ? undefined : toJson(body);
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const resp = await fetch(`${baseUrl.replace(/\/$/, "")}${path}`, {
      method,
      // The run token must not follow a Location to another host.
      redirect: "manual",
      signal: controller.signal,
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json",
        "X-Toft-Runner-Version": CONTRACT_VERSION,
      },
      body: json,
    });

    refuseRedirect(resp, path);
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
  // Private fields, not properties: the job token is a bearer credential and
  // the output URL a presigned one carrying signed object-store credentials,
  // and `console.log(toft.run)` lands in a job log anyone who can run
  // `toft job logs` reads. Non-enumerable own properties kept them out of
  // console.log but not out of `Object.getOwnPropertyDescriptors(run)`, which
  // a generic serializer or debugger walks; no reflection reaches a #field.
  // toJSON and the inspect form below say what there is without either.
  #baseUrl;
  #token;
  #outputUri;

  constructor({ baseUrl, token, id } = {}) {
    this.#baseUrl = baseUrl ?? process.env.TOFT_API_URL;
    this.#token = token ?? process.env.TOFT_JOB_TOKEN;
    this.id = id ?? process.env.TOFT_RUN_ID;
    /** The job this run is of — its `[jobs.<name>]` in `toft.toml`. */
    this.job = process.env.TOFT_JOB_NAME ?? "";
    /**
     * The run this one re-ran, or null. A rerun carries the same params; a job
     * that must not repeat a side effect checks this.
     */
    this.rerunOf = process.env.TOFT_RERUN_OF || null;
    this.#outputUri = process.env.TOFT_OUTPUT_URI ?? "";
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

    if (!this.#baseUrl || !this.#token || !this.id) {
      throw new ToftError(
        "not running inside a Toft job — TOFT_API_URL, TOFT_JOB_TOKEN " +
          "and TOFT_RUN_ID are set by the runner",
        {
          offPlatform: true,
          body: {
            code: "off_platform",
            retryable: false,
            hint: "read toft.run, which is null outside a job, rather than constructing a Run",
          },
        },
      );
    }
  }

  static fromEnv() {
    noMoreArgs("Run.fromEnv()", arguments, 0, "Run.fromEnv(), or read toft.run");
    return new Run();
  }

  /**
   * The presigned URL `writeOutput` PUTs to. A credential: never log it. An
   * accessor over a private field, so it is on no instance's own properties.
   */
  get outputUri() {
    return this.#outputUri;
  }

  set outputUri(value) {
    this.#outputUri = value ?? "";
  }

  /**
   * The run's facts, with no credential: neither the job token nor the
   * output URL, only whether there is somewhere to write output.
   */
  toJSON() {
    return {
      id: this.id,
      job: this.job,
      started_by: this.trigger,
      trigger_role: this.triggerRole,
      rerun_of: this.rerunOf,
      params: this.params,
      scheduled_for: this.scheduledForText,
      deadline: process.env.TOFT_DEADLINE || null,
      has_output_location: this.outputUri !== "",
    };
  }

  /** What `console.log` and `util.inspect` print: the toJSON shape, no secret. */
  [inspect.custom](depth, options, inspectFn = inspect) {
    return `Run ${inspectFn(this.toJSON(), options)}`;
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
   * a few words the status page shows under the bar. A percent outside 0–100,
   * or not a number, is `bad_request` before any request.
   */
  async progress(percent, message) {
    noMoreArgs("progress()", arguments, 2, "await run.progress(40, \"reading ledgers\")");
    // Refused before any request, as Python, Go and R do: a bar past its end
    // (or NaN, which Math.round keeps) is the caller's mistake, not progress.
    if (typeof percent !== "number" || !Number.isFinite(percent) || percent < 0 || percent > 100) {
      throw refusal("bad_request", `progress is a percentage from 0 to 100, got ${String(percent)}`,
        { hint: "run.progress(40, \"reading ledgers\")" });
    }
    const body = { version: CONTRACT_VERSION, percent: Math.round(percent) };
    if (message) body.message = String(message);
    await runnerRequest(this.#baseUrl, this.#token, "POST", "/api/v1/runner/progress", body);
  }

  /**
   * Write one line to this run's log. Nothing but a write to stdout (stderr
   * with `{ error: true }`) — the runner ships both — and here so a job does
   * not have to know that.
   */
  log(message, options) {
    const { error = false } = takeOptions("log()", options, ["error"], 'run.log("failed", { error: true })');
    (error ? process.stderr : process.stdout).write(`${message}\n`);
  }

  /**
   * Upload this run's output. Also a presigned URL, and also in both
   * execution modes: a filesystem path would work in shared mode and would
   * then make an app written against it unable to move to isolated mode.
   */
  async writeOutput(data) {
    noMoreArgs("writeOutput()", arguments, 1, "await run.writeOutput(data)");
    if (!this.outputUri) {
      throw refusal("no_output", "this run has no output location",
        { hint: "the runner sets TOFT_OUTPUT_URI for every run on the platform — call writeOutput from inside a job" });
    }
    if (
      !(typeof data === "string" || data instanceof Uint8Array || data instanceof ArrayBuffer ||
        ArrayBuffer.isView(data) || data instanceof Blob)
    ) {
      throw refusal("bad_request",
        "writeOutput takes a string or bytes — use writeOutputJson(value) for an object",
        { hint: "run.writeOutputJson(value)" });
    }
    let resp;
    try {
      resp = await fetchStore(this.outputUri, { method: "PUT", body: data });
    } catch (err) {
      if (err instanceof ToftError) throw err;
      throw new ToftError(`uploading output: ${err.message}`);
    }
    if (!resp.ok) {
      const err = storeRefusal("output_unwritable", `uploading output: ${resp.status} ${resp.statusText}`, resp.status);
      err.body.hint = resp.status === 429 || resp.status >= 500
        ? "the object store refused for now — write the output again"
        : "the run's output URL was refused, which no retry inside this run changes — rerun the job, and tell an administrator if it recurs";
      throw err;
    }
  }

  async writeOutputJson(value) {
    noMoreArgs("writeOutputJson()", arguments, 1, "await run.writeOutputJson(value)");
    await this.writeOutput(toJson(value));
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
 * Toft a private directory under the system temp directory stands in —
 * `toft-scratch-<uid>/<app>`, mode 0700, never the shared temp directory
 * itself — so app code runs unchanged in local development.
 *
 * `parts` are relative and stay inside the root: an absolute part, or one that
 * resolves outside it (`"..", "..", "etc"`), is `bad_request` before anything
 * is created.
 */
export function scratchDir(...parts) {
  checkScratchParts("scratchDir()", parts, 'scratchDir("uploads")');
  const root = resolve(process.env.TOFT_SCRATCH_DIR || localScratchRoot());
  const path = resolve(root, ...parts);
  const rel = relative(root, path);
  if (rel === ".." || rel.startsWith(`..${sep}`) || isAbsolute(rel)) {
    throw refusal("bad_request",
      `scratchDir(${parts.map((p) => JSON.stringify(p)).join(", ")}) resolves outside the scratch root`,
      { hint: "use relative parts with no '..' that climbs out of the scratch root" });
  }
  mkdirSync(path, { recursive: true, mode: 0o700 });
  return path;
}

// Relative path parts and nothing else, refused under the name of the call the
// caller made: sessionScratchDir's parts said "scratchDir() takes …" (BB-083).
function checkScratchParts(call, parts, example) {
  for (const part of parts) {
    if (typeof part !== "string" || isAbsolute(part) || part.includes("\0")) {
      throw refusal("bad_request",
        `${call} takes relative path parts, got ${JSON.stringify(part) ?? typeof part}`,
        { hint: `name a subpath inside the scratch root, e.g. ${example}` });
    }
  }
}

/**
 * Off the platform: a directory of this user's own under the system temp
 * directory, per app (the working directory's name), created 0700. The shared
 * temp directory is world-writable, so a name another account can predict is
 * checked rather than trusted: a symlink, or a directory somebody else owns,
 * is refused instead of written into.
 */
function localScratchRoot() {
  let uid = "user";
  try {
    uid = String(process.getuid?.() ?? userInfo().username);
  } catch {
    /* no user database; the fixed fallback is still private by mode */
  }
  const userRoot = join(tmpdir(), `toft-scratch-${uid}`);
  mkdirSync(userRoot, { recursive: true, mode: 0o700 });
  const st = lstatSync(userRoot);
  const mine = typeof process.getuid !== "function" || st.uid === process.getuid();
  if (!st.isDirectory() || st.isSymbolicLink() || !mine || (st.mode & 0o077) !== 0) {
    throw refusal("bad_request",
      `${userRoot} is not a private directory of this user's — refusing to use it as scratch`,
      { hint: "remove it, or set TOFT_SCRATCH_DIR to a directory of your own" });
  }
  const app = (process.env.TOFT_APP_SLUG || basename(process.cwd()) || "app").replace(/[^A-Za-z0-9._-]/g, "_");
  return join(userRoot, app === "." || app === ".." ? "app" : app);
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
      {
        body: {
          code: "bad_request",
          retryable: false,
          hint: "pass the X-Toft-Session header, or toft.newSessionId() kept in your own cookie for an anonymous viewer",
        },
      },
    );
  }
  checkScratchParts("sessionScratchDir()", parts, 'sessionScratchDir(sid, "uploads")');
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
  noMoreArgs("newSessionId()", arguments, 0, "const sid = toft.newSessionId()");
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
  RUN_STATUSES,
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
  RUN_STATUSES,
  TERMINAL_STATUSES,
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

