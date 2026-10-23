/**
 * What the caller in front of this request may do, in one word.
 *
 *   app.post("/rebuild", (req, res) => {
 *     const c = toft.caller(req.headers);
 *     if (!c.atLeast(toft.ROLE_EDITOR)) return res.status(403).send("not for you");
 *     …
 *   });
 *
 * Toft puts a signed role on every request it forwards, so an app that
 * wants to protect an expensive endpoint no longer has to choose between
 * exposing it to everyone who can open the page and asking the platform who the
 * viewer is. The three person-words are the ones the Sharing tab granted from —
 * "owner", "editor", "viewer" — plus "anonymous" for a visitor who is not signed
 * in, "system" for Toft firing a scheduled job, and "app" for the app's
 * own credential.
 *
 * **A role is not an identity and it is not domain authority.** It says what
 * this caller may do to *this deployment in Toft* — redeploy it, edit its
 * jobs, change its schedules. "Owners may approve invoices" is a rule about
 * invoices and belongs on a group: see `toft.who`. Nothing here names a person.
 *
 * **This is the app's decision, not the platform's.** Toft enforces nothing
 * new: it tells you, you refuse. Where the *platform* decides — starting a job,
 * running a scheduled job — it re-resolves live and never reads this header.
 *
 * **A missing or unverifiable header is `unknown`, and `atLeast` is false for
 * it.** Absent key, absent header, bad signature and expired all give one
 * answer, deliberately: four branches would be four ways to get a refusal wrong.
 * The one deployment where the header is missing — a health check, a direct
 * port, a laptop — is the one nobody configured, so it must refuse.
 *
 * **Verify on arrival.** The assertion is about one request and is good for two
 * minutes. For a WebSocket the headers arrive at the handshake and the
 * connection lasts hours, so read the role at the handshake and hold your own
 * decision from there — a change of role during a long-held connection does not
 * reach you. The platform's own enforcement is unaffected; the app's window is
 * the app's.
 *
 * No dependencies: `crypto` is Node's own, which is the whole reason the
 * signature is an HMAC rather than something better.
 */

import { createHmac, timingSafeEqual } from "node:crypto";

/**
 * The request header the Toft proxy sets on every request it forwards, and
 * on every scheduled firing it makes.
 */
export const ROLE_HEADER = "X-Toft-Role";

/**
 * The environment variable Toft injects the verification key into. Per app
 * and per start: a redeploy replaces it, which is why nothing signed for the
 * previous release verifies against this one.
 */
export const ENV_ROLE_KEY = "TOFT_ROLE_KEY";

/** Every failure: no header, no key, a bad signature, an expired blob. Never privileged. */
export const ROLE_UNKNOWN = "unknown";
/** A visitor who is not signed in to Toft. Only a public app sees it. */
export const ROLE_ANONYMOUS = "anonymous";
/**
 * What this person may do to this deployment. A Toft administrator with no
 * grant on the app reads as `viewer` — the header carries no admin bit, and an
 * app that needs to know is asking about a person, which is what `toft.who` is for.
 */
export const ROLE_VIEWER = "viewer";
export const ROLE_EDITOR = "editor";
export const ROLE_OWNER = "owner";
/**
 * Toft acting on its own: today, a scheduled job firing on its schedule.
 * A *manual* firing of the same schedule carries the role of whoever pressed the
 * button.
 */
export const ROLE_SYSTEM = "system";
/** The app acting as itself — an app-started job's callback. */
export const ROLE_APP = "app";

const ROLE_VERSION = "v1";

/** anonymous < viewer < editor < owner. `system` and `app` are not on it. */
const PERSON_RANK = {
  [ROLE_ANONYMOUS]: 0,
  [ROLE_VIEWER]: 1,
  [ROLE_EDITOR]: 2,
  [ROLE_OWNER]: 3,
};

/** Who is asking, as a role rather than a person. */
export class Caller {
  constructor(role) {
    /**
     * One of the seven constants above and never anything else — a word this SDK
     * does not know is `unknown`, so an install running ahead of an app's SDK
     * refuses rather than handing app code a value it has never branched on.
     */
    this.role = role;
  }

  /**
   * Whether this caller holds at least the given role on the person ladder.
   *
   * **It answers only about the ladder.** `system` and `app` are not on it and
   * are false for every rung: they are not more or less authorized than a
   * person, they are a different kind of caller, and ranking Toft's
   * scheduler above an owner would be a guess this SDK has no business making.
   * An endpoint that serves both a person and its own schedule says so:
   *
   *   if (c.role !== toft.ROLE_SYSTEM && !c.atLeast(toft.ROLE_EDITOR)) return refuse();
   *
   * `unknown` is false for everything, `anonymous` included — an unverifiable
   * caller is not "at least a stranger", it is nobody this app can say anything
   * about.
   *
   * **A rung does not mean a human.** A machine client is an account granted
   * on this app, and it satisfies `atLeast` exactly as a person with the same
   * grant does. That is the point of the word
   * — the caller may do what it says — and it is why nothing here answers "is a
   * human in front of me". See `isPerson`.
   */
  atLeast(role) {
    const mine = PERSON_RANK[this.role];
    const want = PERSON_RANK[role];
    return mine !== undefined && want !== undefined && mine >= want;
  }

  /**
   * On the person ladder — `anonymous`, `viewer`, `editor` or `owner`. False for
   * `system`, `app`, `unknown`.
   *
   * **It does not mean a human, and it never checked for one.** This has always
   * been membership of the ladder and nothing more; what changed is that a rung
   * is reached by something other than a person: a machine client is an
   * account granted on this app, standing on the ladder like anyone.
   *
   * **This platform cannot tell you a human is in front of you.** Nothing here
   * can, and nothing here pretends to — the honest question an app can ask is
   * what the caller may do, which is what the word says.
   */
  get isPerson() {
    return PERSON_RANK[this.role] !== undefined;
  }

  /** Toft firing a scheduled job on its schedule. A manual firing is a person. */
  get isSystem() {
    return this.role === ROLE_SYSTEM;
  }

  /**
   * Above anonymous on the person ladder. False for an anonymous visitor.
   *
   * Like `isPerson`, a fact about the ladder rather than about a human: an app
   * key minted with `viewer` or `editor` is true here, and no account was signed
   * into to produce it.
   *
   * This is the one bit a public app gains that it did not have: it can tell a
   * reader with a rung from a stranger. It names nobody — but a log line per
   * request would build a record of which of your readers have Toft
   * accounts, which is worth deciding about rather than doing by default.
   */
  get signedIn() {
    const rank = PERSON_RANK[this.role];
    return rank !== undefined && rank > 0;
  }

  /** Safe to log: a role, never a person and never the blob. */
  toString() {
    return `<toft.Caller ${this.role}>`;
  }
}

/**
 * Read the role off the request headers this app is serving.
 *
 * Never throws. Off-platform, or on a request that did not come through the
 * gateway, the role is `unknown` and every `atLeast` is false.
 *
 * `headers` may be Node's lowercased `req.headers`, a `Headers`, or any object —
 * the lookup is case-insensitive either way, because a framework's own header
 * type is where a case-sensitive read becomes a bug on somebody else's stack.
 */
export function caller(headers) {
  return new Caller(verifyRole(process.env[ENV_ROLE_KEY] ?? "", headerValue(headers, ROLE_HEADER), Date.now()));
}

function headerValue(headers, name) {
  if (!headers) return "";
  const wanted = name.toLowerCase();
  if (typeof headers.get === "function") {
    return headers.get(name) ?? headers.get(wanted) ?? "";
  }
  for (const [key, value] of Object.entries(headers)) {
    if (key.toLowerCase() === wanted) {
      return Array.isArray(value) ? (value[0] ?? "") : (value ?? "");
    }
  }
  return "";
}

/**
 * The app side of the platform's mint, and the two are one specification:
 * `v1.<role>.<base64url app id>.<expiry>.<mac>`, where the MAC is HMAC-SHA256
 * over `"toft-role\0"` plus the payload, keyed with TOFT_ROLE_KEY,
 * base64url without padding.
 *
 * The app id is not checked here and does not need to be: the key is derived per
 * app, so a blob minted for another app fails the MAC. Checking it as well would
 * need this process to know its own id — a second thing to get wrong for no
 * property gained.
 *
 * Exported for the test that compares this MAC against the platform's over the
 * same input. App code calls `caller()`.
 */
export function verifyRole(key, blob, nowMs) {
  if (!key || !blob) return ROLE_UNKNOWN;
  const idx = blob.lastIndexOf(".");
  if (idx < 0) return ROLE_UNKNOWN;

  const payload = blob.slice(0, idx);
  const sig = blob.slice(idx + 1);
  const want = createHmac("sha256", key)
    .update(Buffer.concat([Buffer.from("toft-role"), Buffer.from([0])]))
    .update(payload)
    .digest("base64url");
  // Constant time, and before anything is parsed: structure decoded ahead of
  // authentication is how a verifier grows a second entry point. timingSafeEqual
  // throws on a length mismatch, which is itself not secret — a signature of the
  // wrong length is not a signature.
  const got = Buffer.from(sig);
  const expected = Buffer.from(want);
  if (got.length !== expected.length || !timingSafeEqual(got, expected)) return ROLE_UNKNOWN;

  const fields = payload.split(".");
  if (fields.length !== 4 || fields[0] !== ROLE_VERSION) return ROLE_UNKNOWN;
  const expiry = Number.parseInt(fields[3], 10);
  if (!Number.isFinite(expiry) || expiry * 1000 <= nowMs) return ROLE_UNKNOWN;

  // A word this SDK does not know is unknown rather than passed through. The app
  // holds this key and could sign anything with it; app code switching on six
  // values must not be handed a seventh, whoever minted it.
  const role = fields[1];
  if (PERSON_RANK[role] !== undefined || role === ROLE_SYSTEM || role === ROLE_APP) return role;
  return ROLE_UNKNOWN;
}
