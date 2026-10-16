/**
 * The app's one JSON document — settings and small state that outlive a
 * restart, a redeploy and a rollback, and belong to nobody's session.
 *
 *   const settings = (await toft.data.get()) ?? { period: "monthly" };
 *   await toft.data.set({ ...settings, period });
 *
 * It sits between the two things that already existed and neither of which fits
 * a handful of settings: `toft.scratchDir()` is wiped on every start, and mapped
 * storage is durable but is an administrator's separate decision per app. This
 * needs nothing switched on — every app has it, because it holds only what the
 * app put there and reaches nothing outside the install.
 *
 * **One document, not a store.** No keys, no listing, no queries, and a cap of
 * 256 KiB. An app with more than that has outgrown the feature rather than hit
 * a limit worth raising, and the platform's refusal says so.
 *
 * **Never written and cleared are the same state, and both differ from `{}`.**
 * `get()` answers `null` for either, which is why an app can write `{}` and
 * mean it.
 *
 * **Two writers need `update()`.** A `set()` is last-write-wins, which is right
 * for the single-writer app and silently loses a change when a job and the web
 * process both save. `update()` reads, applies your change, and writes it back
 * under the version it read — retrying if somebody got there first, so the
 * other writer's change survives instead of being overwritten.
 */

import { ToftError, transport } from "./transport.js";

/**
 * The document changed under a write that named the version it meant to
 * replace. Carries the version that *is* stored, so recovery is one read rather
 * than a guess.
 */
export class VersionConflict extends ToftError {
  constructor(message, options = {}) {
    super(message, options);
    this.name = "VersionConflict";
    this.version = options.body?.version ?? 0;
  }
}

/** How many times `update()` re-reads and re-applies before giving up. */
const DEFAULT_ATTEMPTS = 5;

/** One read of the document, with the platform's two facts about it. */
export class Document {
  constructor(raw) {
    const data = raw ?? {};
    /** The document, or `null` when the app has never written one. */
    this.data = data.data ?? null;
    /** 0 when nothing is stored; the compare-and-set token otherwise. */
    this.version = data.version ?? 0;
    /** When it was last written, or `null`. */
    this.updatedAt = data.updated_at ?? null;
    /** Its size in bytes, as the platform counted it against the cap. */
    this.bytes = data.bytes ?? 0;
  }

  /** True when nothing has been saved, or it was cleared. */
  get empty() {
    return this.data === null;
  }
}

/** The app's one JSON document. */
export class Data {
  /** The document, or `null` when the app has never written one. */
  async get() {
    return (await this.read()).data;
  }

  /** The document *and* its version — what `update()` is built on. */
  async read() {
    return new Document(await transport().get("/api/v1/app/data"));
  }

  /**
   * Replace the document.
   *
   * `ifVersion` makes it conditional: the write happens only if that is still
   * the stored version, and throws `VersionConflict` otherwise. `0` means "only
   * if nothing is stored", which is how an app claims first write without a
   * separate call.
   */
  async set(document, { ifVersion } = {}) {
    // The one thing checked locally, and not a duplicated platform limit: an
    // `undefined` here would be serialized as `{}` and quietly wipe the app's
    // settings, which is a wrong request rather than a refused one.
    if (document === null || typeof document !== "object" || Array.isArray(document)) {
      throw new ToftError(
        "app data holds one JSON object — an array or a bare value has nowhere to " +
          'grow a field later, so wrap it, e.g. { items: [...] }.',
      );
    }
    const headers = ifVersion === undefined ? undefined : { "If-Match": `"${ifVersion}"` };
    const raw = await typed(() => transport().putJson("/api/v1/app/data", document, { headers }));
    // **The write's answer carries what was written.** The platform's reply to
    // a PUT is deliberately metadata only — version, timestamp, size — rather
    // than the document echoed back, so without this the handle came back with
    // `data: null`: the value this API reserves for "never written", handed to
    // a caller who had just written. Filled from the document that went out
    // rather than by a second read, which would cost a round trip and could
    // answer with somebody else's later write.
    const doc = new Document(raw);
    doc.data = document;
    return doc;
  }

  /**
   * Read, change, write back — without losing a concurrent writer's change.
   *
   * `mutator` is given the current document (`{}` when there is none) and may
   * either mutate it or return a replacement. The write is conditional on the
   * version that was read, so if somebody else wrote in between, this re-reads
   * and applies your change again rather than overwriting theirs.
   *
   *   await toft.data.update((s) => { s.runs = (s.runs ?? 0) + 1; });
   *
   * Use `read()`/`set()` directly when "there was nothing here" has to be told
   * apart from "there was an empty document" — that distinction is the one
   * thing `{}` papers over.
   */
  async update(mutator, { attempts = DEFAULT_ATTEMPTS } = {}) {
    let last;
    for (let attempt = 0; attempt < attempts; attempt++) {
      const current = await this.read();
      const working = current.data ?? {};
      const next = (await mutator(working, current)) ?? working;
      try {
        return await this.set(next, { ifVersion: current.version });
      } catch (err) {
        if (!(err instanceof VersionConflict)) throw err;
        last = err;
      }
    }
    throw new ToftError(
      `app data changed under ${attempts} attempts to update it — something is ` +
        `writing it continuously`,
      { status: last?.status, body: last?.body ?? {} },
    );
  }

  /**
   * Throw the document away, putting the app back to never-having-written one.
   *
   * Not the same as writing `{}`, deliberately: an app that cleared its settings
   * and one that saved an empty object mean different things, and the next read
   * tells them apart.
   */
  async clear({ ifVersion } = {}) {
    const headers = ifVersion === undefined ? undefined : { "If-Match": `"${ifVersion}"` };
    const raw = await typed(() => transport().del("/api/v1/app/data", headers));
    return Boolean(raw?.deleted);
  }
}

/**
 * Give a failed precondition a type, so `update()` can retry it and app code
 * can catch it, rather than matching on a status code.
 */
async function typed(call) {
  try {
    return await call();
  } catch (err) {
    if (err instanceof ToftError && err.code === "version_conflict") {
      throw new VersionConflict(err.message, {
        status: err.status,
        retryAfter: err.retryAfter,
        body: err.body,
      });
    }
    throw err;
  }
}
