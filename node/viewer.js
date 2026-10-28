/**
 * The app acting for whoever is looking at the page.
 *
 *   const me = toft.viewer(req.headers);          // Express, Fastify, Hono
 *   const run = await me.jobs.start("reconcile", { params: { month } });
 *   const recent = await me.jobs.mine({ job: "reconcile", limit: 20 });
 *   return toft.jobs.respond(run, req);             // answer now; never wait here
 *
 * **The app never asserts who the viewer is.** What it relays is a blob the
 * platform signed over `(user, app, session)` on its way through the gateway.
 * The app cannot mint one, cannot alter one, and cannot read who is inside it —
 * the same contract `X-Toft-Session` already has: a correlation id, not
 * identity.
 *
 * What falls out is the valuable part: the platform holds the real principal at
 * the moment it decides. Whether this person may start this job is evaluated
 * against *their* permissions, not against the app's claim about them, so an
 * app cannot launder a viewer into authority they do not have. It works in the
 * other direction too — `mine()` answers a question *about* the viewer without
 * disclosing the viewer.
 *
 * **A missing header throws.** It never falls back to the app acting as itself:
 * a silent downgrade would move a run out of a person's name and into nobody's,
 * and nobody would notice until an audit. The usual cause is the app being
 * reached outside the Toft gateway — a health check, a direct port, a
 * local dev server.
 *
 * **It expires with the session it came from.** A token on a short independent
 * clock would break the archetypal app, which holds its handshake headers for
 * hours; when this one does expire the person signs in again, and nothing is
 * broken.
 */

import { Jobs, requestDeadline } from "./jobs.js";
import { ToftError, transport } from "./transport.js";

/**
 * How long a resolved identity is reused for the same blob.
 *
 * A per-render call must not become a request per render, and 60 seconds is the
 * honest trade rather than a tuned number. What it buys and what it costs: **a
 * role change takes effect in the app within the TTL, not instantly.** Where the
 * *platform* enforces — starting a job, firing a schedule — it
 * re-resolves live and this cache is irrelevant. Where the *app* enforces, the
 * app is the enforcer and the window is the app's to accept or shorten.
 * `who(headers, { refresh: true })` bypasses it.
 */
export const IDENTITY_TTL_MS = 60_000;

// Bounded as well as expiring: an app serving many people would otherwise hold
// one entry per session for the life of the process. Clearing the whole map when
// it fills is deliberate — an LRU would be machinery in aid of a cache whose
// entries are worth a minute each.
const IDENTITY_CACHE_MAX = 512;
const identityCache = new Map();

/**
 * The request header the Toft proxy sets on its way to the app.
 *
 * Express lowercases header names in `req.headers`; lookup here is
 * case-insensitive either way, because a case-sensitive one would be a bug that
 * only shows up on somebody else's framework.
 */
export const ACT_FOR_HEADER = "X-Toft-Act-For";

/**
 * One person, as a scoping capability rather than a fact the app holds.
 *
 * The capability half is unchanged and is still the default: everything an app
 * wants to do *with* a viewer — start work in their name, list what they
 * started — the platform does on its behalf, holding the real principal at the
 * moment it decides.
 *
 * `identity()` is the deliberate exception, for the question the app must
 * answer itself: may this person see the approvals tab. The platform is not in
 * the loop there, so the app is the enforcer and needs the fact. Off until an
 * administrator enables it for this app.
 */
export class Viewer {
  constructor(blob, { requestDeadline = null } = {}) {
    // The job surface, with this viewer attached — and the request it came in
    // on, so a wait() that would outlive that request rejects.
    this.jobs = new Jobs({ [ACT_FOR_HEADER]: blob }, { requestDeadline });
    // Not enumerable, so a console.log of a Viewer cannot spill a bearer
    // credential for somebody's session into a log line.
    Object.defineProperty(this, "_blob", { value: blob, enumerable: false });
  }

  /**
   * Resolve who this viewer is, or `null` if there is nobody to name.
   *
   * The same call `who()` makes, reached from a `Viewer` an app already built
   * to start a job.
   */
  async identity({ refresh = false } = {}) {
    return resolveIdentity(this._blob, refresh);
  }

  toString() {
    return "[Viewer: a signed-in person; the app cannot see who]";
  }
}

/**
 * Who is looking at this app, or `null` if there is nobody to name.
 *
 *   const me = await toft.who(req.headers);          // Identity | null
 *   if (me?.role === "owner" || me?.isAdmin) renderAdminTab();
 *   if (me?.groupNames.includes("finance")) renderLedger();
 *
 * **This never throws for an absent viewer, and `viewer()` still does.** The
 * two answer different questions — *who is this* and *act as this person* — and
 * only the first can honestly answer "nobody". A public app rendering "sign in
 * to see your reports" should not have to wrap the normal case in a try/catch;
 * an app about to start a job in somebody's name should never silently start it
 * in nobody's.
 *
 * `null` means nobody is there: an anonymous visitor, or somebody whose access
 * was revoked while their page stayed open. **It never means "not allowed to
 * ask"** — that throws, naming the setting and saying an administrator enables
 * it. An app that would rather not throw checks
 * `toft.app.can("viewer.identity")` at startup.
 *
 * `role` is `owner`, `editor` or `viewer` and answers *what may this person do
 * to this deployment in Toft* — not domain authority. A rule like "the
 * finance team may approve" belongs on `groupNames`. `isAdmin` is separate
 * because Toft hands an administrator effective ownership of every app, so
 * gating on `role === "owner"` alone would grant every administrator the thing
 * with no way for the app to refuse. `email` is `null` on a public app.
 */
export async function who(headers, { refresh = false } = {}) {
  const blob = findHeader(headers, ACT_FOR_HEADER);
  if (!blob) return null;
  return resolveIdentity(blob, refresh);
}

// Keyed on the blob rather than a user id, because the blob is all the app has
// and two people are never behind one — it is signed over (user, app, session).
async function resolveIdentity(blob, refresh) {
  if (!refresh) {
    const hit = identityCache.get(blob);
    if (hit && hit.expires > Date.now()) return hit.identity;
  }

  const identity = shapeIdentity(
    await transport().get("/api/v1/app/viewer", { [ACT_FOR_HEADER]: blob }),
  );

  if (identityCache.size >= IDENTITY_CACHE_MAX) identityCache.clear();
  identityCache.set(blob, { expires: Date.now() + IDENTITY_TTL_MS, identity });
  return identity;
}

// 204 arrives as a null body: the app's own credential was valid and the
// question was answerable, and the answer is that there is no viewer.
function shapeIdentity(body) {
  if (!body) return null;
  const groups = (body.groups ?? []).map((g) => ({ id: g.id ?? "", name: g.name ?? "" }));
  return {
    id: body.id ?? "",
    name: body.name ?? "",
    email: body.email ?? null,
    avatarUrl: body.avatar_url ?? null,
    role: body.role ?? "",
    isAdmin: Boolean(body.is_admin),
    groups,
    groupNames: groups.map((g) => g.name),
  };
}

/** Drop every cached identity. For tests; app code passes `refresh`. */
export function resetIdentityCache() {
  identityCache.clear();
}

/**
 * Build a `Viewer` from the request headers the app is serving.
 *
 * Accepts anything header-shaped: a plain object, Express's `req.headers`, a
 * `Headers` instance, a `Map`.
 *
 * The request's `X-Toft-Request-Deadline` is kept too, and every run started or
 * read through this viewer carries it: that is what lets `run.wait()` refuse to
 * outlive the request (`would_outlive_request`).
 */
export function viewer(headers) {
  const blob = findHeader(headers, ACT_FOR_HEADER);
  if (!blob) {
    throw new ToftError(
      `no ${ACT_FOR_HEADER} header on this request, so there is no viewer to act for. ` +
        `Toft's proxy sets it on every browser request through the gateway, an anonymous ` +
        `visitor to a public app included (that header names nobody, and a start in its ` +
        `name is refused viewer_anonymous), so its absence means ` +
        `the request came with an API key (the gateway authorizes a key but asserts no ` +
        `person to the app — \`toft fetch --session\` is a signed-in request), or the app ` +
        `was reached outside the gateway — a health check, a direct port, or a local ` +
        `development server. Use toft.jobs to act as the app itself, ` +
        `deliberately, rather than treating this as the same thing.`,
      {
        body: {
          code: "no_viewer",
          retryable: false,
          hint: "use toft.jobs.start(...) to start it as the app, or toft.jobs.list(...) for its runs",
        },
      },
    );
  }
  return new Viewer(blob, { requestDeadline: requestDeadline(headers) });
}

/** One header out of whatever the framework calls its header mapping, or "". */
export function findHeader(headers, name) {
  if (!headers) return "";
  const wanted = name.toLowerCase();

  // Headers and Map both have a get(); Headers is already case-insensitive.
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
