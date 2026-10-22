/**
 * The HTTP transport for `/api/v1/app/*`.
 *
 * No dependencies, on purpose. This runs inside somebody's app beside their own
 * tree, and a version conflict between our HTTP library and theirs would be our
 * bug appearing in their build. Node 18's built-in `fetch` is enough.
 *
 * Two things here are worth reading before changing them, and they are the same
 * two the Python transport carries — the two libraries have to behave
 * identically or an app written in one observes a platform the other cannot.
 *
 * **The retry is exactly one, and it is not a retry policy.** The platform's own
 * queue does the real work. This covers the far narrower case of a request never
 * reaching a handler: a 429 from the rate limiter, or a 5xx from in front of it.
 *
 * **A retried request reuses its idempotency key.** Without that, this retry is
 * precisely the thing that starts the same job twice.
 */

const DEFAULT_TIMEOUT_MS = 30_000;
const DEFAULT_RETRY_AFTER_MS = 2_000;

/** The app contract version this SDK speaks, sent on every call. */
export const APP_CONTRACT_VERSION = "1";
export const CONTRACT_HEADER = "X-Toft-App-Contract";

export const ENV_APP_TOKEN = "TOFT_APP_TOKEN";
export const ENV_API_URL = "TOFT_API_URL";

// Not read for their values here, only to tell one absence from another: inside
// a job the app token is *meant* to be missing, and the run id is exactly the
// condition "we are inside a job".
export const ENV_RUN_ID = "TOFT_RUN_ID";
export const ENV_JOB_NAME = "TOFT_JOB_NAME";

/**
 * An error from the platform.
 *
 * `status` tells "you asked for something impossible" from "try again later".
 * `body` is the decoded refusal, because some of them carry figures an app can
 * render — a full queue says how many runs are in flight and what the limit is
 * — and flattening those into a message string throws away the only part that
 * could go on a page. `code` is the stable name to branch on, rather than prose
 * that may be reworded.
 *
 * **Whether to try again is the platform's call, not this class's.** Every
 * error body the platform writes carries `code`, `retryable` and, for codes
 * that have one, `hint` — the next step in its own words. `retryable` returns
 * the platform's answer when the body has one, because only the handler knows
 * that a 503 means "this install is configured without it, forever" or that a
 * 409 means "wait for a run to finish". The status rule is the fallback for
 * refusals that never reached a handler: a transport failure, a proxy's HTML
 * page, an install older than the envelope.
 */
export class ToftError extends Error {
  constructor(message, { status, retryAfter, body, offPlatform } = {}) {
    super(message);
    this.name = "ToftError";
    this.status = status;
    this.retryAfter = retryAfter;
    this.body = body ?? {};
    /**
     * Set on the one refusal that no retry can help: a variable the platform
     * injects is not there. Without it the missing-environment error looks
     * exactly like a network failure — no status — and this transport slept two
     * seconds before raising the identical error a second time. The environment
     * does not arrive while a process waits.
     */
    this.offPlatform = Boolean(offPlatform);
  }

  get code() {
    return this.body.code ?? "";
  }

  get retryable() {
    if (this.offPlatform) return false;
    if (typeof this.body.retryable === "boolean") return this.body.retryable;
    return this.#statusRetryable();
  }

  /**
   * The platform's next step for this refusal — "mint a new key", "an
   * administrator has to lift the lock" — or undefined when it sent none.
   * Written for whoever reads the failure, so an app logging the error should
   * log this too.
   */
  get hint() {
    return typeof this.body.hint === "string" && this.body.hint ? this.body.hint : undefined;
  }

  #statusRetryable() {
    return this.status === undefined || this.status >= 500 || this.status === 429;
  }

  /**
   * Whether the transport's single built-in retry fires. Narrower than
   * `retryable` on purpose: that retry covers a request that never reached a
   * handler — a rate limiter, a gateway — so the platform can veto it (a 503
   * saying "not configured" is not slept over) but cannot widen it to a 4xx. A
   * 409 the platform calls retryable means "wait for something", and how long
   * is the caller's loop to decide, not two seconds here.
   */
  get autoRetry() {
    return this.retryable && this.#statusRetryable();
  }
}

/**
 * Read an injected variable, or throw naming what sets it.
 *
 * The message names the variable rather than describing the situation, because
 * the fix is always the same: run this on Toft. There is deliberately no
 * dry-run mode — an SDK that quietly did nothing off-platform is a trap that
 * only springs in production.
 */
export function require_(name) {
  const value = (process.env[name] ?? "").trim();
  if (value) return value;
  throw new ToftError(missing(name), { offPlatform: true, body: { code: missingCode(name), retryable: false } });
}

/**
 * The code missing()'s sentence carries: job_send_only — the platform's own
 * word for the same rule — when a job asked for the app token, off_platform
 * otherwise. Neither is helped by trying again.
 */
function missingCode(name) {
  return name === ENV_APP_TOKEN && (process.env[ENV_RUN_ID] ?? "").trim() ? "job_send_only" : "off_platform";
}

/**
 * Why the variable is absent, in the two cases that differ.
 *
 * **The app token missing inside a job is not a broken deploy.** A job workload
 * is given none, deliberately: the app's credential belongs to the process
 * serving its requests, and a job serves none. The generic sentence is actively
 * misleading there — it tells an author running a deployed app on the platform
 * that they are not running on the platform, and sends them looking for a
 * missing variable, a broken deploy or a platform bug. The answer is
 * architectural, so it has to be in the sentence.
 *
 * `TOFT_RUN_ID` is what says we are inside a job; the runner exports it
 * for every run and for nothing else. Same branch, same wording, in all four
 * SDKs — an author who moves language must not meet a different explanation of
 * one platform rule.
 */
function missing(name) {
  const runId = (process.env[ENV_RUN_ID] ?? "").trim();
  if (name === ENV_APP_TOKEN && runId) {
    const job = (process.env[ENV_JOB_NAME] ?? "").trim();
    const where = job ? `job "${job}", run ${runId}` : `run ${runId}`;
    return (
      `${name} is not set because this is a job (${where}), and a job is deliberately ` +
      `given no app token — this is not a broken deploy. toft.data, toft.app and toft.notify ` +
      `belong to the process serving this app's requests, and a job serves none. Two ways ` +
      `round it: do this from the service process, or carry the result out of the run with ` +
      `run.setResult(...) or run.writeOutputJson(...) and let an endpoint fold it in.`
    );
  }
  return (
    `${name} is not set. This call needs the environment Toft injects into a ` +
    `deployed app — it only works in an app running on the platform.`
  );
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export class AppTransport {
  constructor({ baseUrl, token, timeoutMs = DEFAULT_TIMEOUT_MS } = {}) {
    this._baseUrl = baseUrl;
    this._token = token;
    this._timeoutMs = timeoutMs;
  }

  // Built lazily from the environment on first use, so importing the package
  // off-platform never throws — only calling something that needs the platform
  // does.
  get baseUrl() {
    return (this._baseUrl ??= require_(ENV_API_URL)).replace(/\/$/, "");
  }

  // The app's own token in its service process; inside a job, which holds no
  // app token, the run's own credential — accepted by the platform for sending
  // through the app's integrations and nothing else (ADR §108). So `notify`
  // works from a job, and `data` from a job is refused with `job_send_only`.
  get token() {
    if (this._token == null) {
      const app = (process.env[ENV_APP_TOKEN] ?? "").trim();
      const job = (process.env.TOFT_JOB_TOKEN ?? "").trim();
      const inRun = (process.env.TOFT_RUN_ID ?? "").trim() !== "";
      this._token = app || (inRun && job) || require_(ENV_APP_TOKEN);
    }
    return this._token;
  }

  async get(path, headers) {
    return this.#attempt("GET", path, undefined, headers);
  }

  /**
   * PUT a JSON body — one resource replaced, rather than one thing enqueued.
   *
   * No idempotency key, on purpose: a PUT already replaces the same resource
   * whatever the attempt number, so the key that keeps a retried POST from
   * starting two runs has nothing to protect here. `If-Match` is the header
   * that matters instead, and it belongs to the caller.
   */
  async putJson(path, body, { headers } = {}) {
    return this.#attempt("PUT", path, body ?? {}, headers);
  }

  async del(path, headers) {
    return this.#attempt("DELETE", path, undefined, headers);
  }

  async postJson(path, body, { idempotencyKey, headers } = {}) {
    const merged = { ...(headers ?? {}) };
    if (idempotencyKey) merged["Idempotency-Key"] = idempotencyKey;
    return this.#attempt("POST", path, body ?? {}, merged);
  }

  /**
   * POST fields and files as multipart/form-data.
   *
   * Attachments travel this way rather than base64 inside a JSON body: a 10 MiB
   * report becomes a 13 MiB string that then has to be decoded into a second
   * 10 MiB buffer, and paying that on the platform's heap for the tidiness of a
   * single content type is not a trade worth making. Same reasoning, same wire
   * format, as the Python transport — the two libraries have to behave
   * identically or an app written in one observes a platform the other cannot.
   *
   * Node's built-in FormData is used precisely so this stays dependency-free,
   * and the Content-Type is left unset on purpose: `fetch` fills it in with the
   * boundary it generated, and one supplied here would be missing that boundary.
   */
  async postMultipart(path, fields, parts, { idempotencyKey, headers } = {}) {
    const form = new FormData();
    for (const [name, value] of Object.entries(fields ?? {})) {
      if (value !== undefined && value !== null) form.append(name, String(value));
    }
    for (const part of parts ?? []) {
      form.append(
        part.field,
        new Blob([part.content], { type: part.contentType }),
        part.filename,
      );
    }
    const merged = { ...(headers ?? {}) };
    if (idempotencyKey) merged["Idempotency-Key"] = idempotencyKey;
    return this.#attemptForm(path, form, merged);
  }

  async #attemptForm(path, form, headers) {
    try {
      return await this.#onceForm(path, form, headers);
    } catch (err) {
      if (!(err instanceof ToftError) || !err.autoRetry) throw err;
      await sleep(err.retryAfter !== undefined ? err.retryAfter * 1000 : DEFAULT_RETRY_AFTER_MS);
      return this.#onceForm(path, form, headers);
    }
  }

  async #onceForm(path, form, headers) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this._timeoutMs);
    let resp;
    try {
      resp = await fetch(`${this.baseUrl}${path}`, {
        method: "POST",
        redirect: "follow",
        signal: controller.signal,
        headers: {
          Authorization: `Bearer ${this.token}`,
          [CONTRACT_HEADER]: APP_CONTRACT_VERSION,
          ...(headers ?? {}),
        },
        body: form,
      });
    } catch (err) {
      // Our own refusal, raised while composing the request rather than by the
      // network — the lazy `token` getter is the one that does this. Rethrown
      // untouched: wrapping it in "cannot reach Toft at …" buried the real
      // sentence inside a second one that named the wrong problem, and made a
      // refusal no retry can help look like a network failure worth retrying.
      if (err instanceof ToftError) throw err;
      if (err.name === "AbortError") {
        throw new ToftError(`Toft did not answer within ${this._timeoutMs}ms`);
      }
      throw new ToftError(`cannot reach Toft at ${this.baseUrl}: ${err.message}`);
    } finally {
      clearTimeout(timer);
    }
    const text = await resp.text();
    if (!resp.ok) throw errorFromText(resp, text);
    return text ? JSON.parse(text) : null;
  }

  /**
   * GET a route that redirects to an object store, and return the bytes.
   *
   * The redirect is followed by hand. `fetch` would replay this app's platform
   * token to whatever host the store lives on — a credential leak in exchange
   * for one saved option — so the second request carries no Toft headers
   * at all. A presigned URL is already the whole authorization.
   *
   * Resolves with an empty buffer when the platform says there is nothing to
   * fetch; a refusal it can explain (an expired artifact) throws with the
   * platform's own words.
   */
  async follow(path, headers) {
    const resp = await this.#fetch("GET", path, undefined, headers, "manual");
    if (resp.status === 404) return new Uint8Array();
    if (resp.status >= 300 && resp.status < 400) {
      const location = resp.headers.get("Location");
      if (!location) {
        throw new ToftError("the platform redirected without a location");
      }
      const object = await fetch(location);
      if (!object.ok) {
        throw new ToftError(`fetching the object: ${object.statusText}`, {
          status: object.status,
        });
      }
      return new Uint8Array(await object.arrayBuffer());
    }
    if (!resp.ok) throw await errorFrom(resp);
    return new Uint8Array(await resp.arrayBuffer());
  }

  async #attempt(method, path, body, headers) {
    try {
      return await this.#once(method, path, body, headers);
    } catch (err) {
      if (!(err instanceof ToftError) || !err.autoRetry) throw err;
      await sleep(err.retryAfter !== undefined ? err.retryAfter * 1000 : DEFAULT_RETRY_AFTER_MS);
      return this.#once(method, path, body, headers);
    }
  }

  async #once(method, path, body, headers) {
    const resp = await this.#fetch(method, path, body, headers, "follow");
    const text = await resp.text();
    if (!resp.ok) throw errorFromText(resp, text);
    return text ? JSON.parse(text) : null;
  }

  async #fetch(method, path, body, headers, redirect) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this._timeoutMs);
    try {
      return await fetch(`${this.baseUrl}${path}`, {
        method,
        redirect,
        signal: controller.signal,
        headers: {
          Authorization: `Bearer ${this.token}`,
          [CONTRACT_HEADER]: APP_CONTRACT_VERSION,
          ...(body === undefined ? {} : { "Content-Type": "application/json" }),
          ...(headers ?? {}),
        },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
    } catch (err) {
      // Our own refusal, raised while composing the request rather than by the
      // network — the lazy `token` getter is the one that does this. Rethrown
      // untouched: wrapping it in "cannot reach Toft at …" buried the real
      // sentence inside a second one that named the wrong problem, and made a
      // refusal no retry can help look like a network failure worth retrying.
      if (err instanceof ToftError) throw err;
      if (err.name === "AbortError") {
        throw new ToftError(`Toft did not answer within ${this._timeoutMs}ms`);
      }
      throw new ToftError(`cannot reach Toft at ${this.baseUrl}: ${err.message}`);
    } finally {
      clearTimeout(timer);
    }
  }
}

async function errorFrom(resp) {
  return errorFromText(resp, await resp.text());
}

/**
 * Turn the platform's error body into the message a developer reads.
 *
 * Refusals from `/api/v1/app/*` are written for a person and name the remedy.
 * Flattening one into "403 Forbidden" throws away the only part that helps.
 */
export function errorFromText(resp, text) {
  let body = {};
  let detail = text;
  try {
    const parsed = JSON.parse(text);
    if (parsed && typeof parsed === "object") {
      body = parsed;
      detail = parsed.error ?? text;
    }
  } catch {
    /* the body was not JSON; the raw text is the best message available */
  }
  const header = resp.headers.get("Retry-After");
  const retryAfter = header ? Number(header) : undefined;
  return new ToftError(detail || resp.statusText, {
    status: resp.status,
    retryAfter: Number.isFinite(retryAfter) ? retryAfter : undefined,
    body,
  });
}

/**
 * The process-wide transport, built on first use.
 *
 * One connection story for the whole SDK, so `toft.app` and `toft.jobs` cannot end
 * up talking to two different places. `resetTransport` exists for tests;
 * nothing in normal app code calls it.
 */
let shared = null;

export function transport() {
  return (shared ??= new AppTransport());
}

export function resetTransport(t = null) {
  shared = t;
}
