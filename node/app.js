/**
 * The app's view of itself, and the durable resources mapped to it.
 *
 * `toft.app` answers two different kinds of question and it is worth keeping them
 * apart. *Who am I* — id, slug, name, execution mode — is for the app's own
 * logs and for building links back to itself. *What may I do* — `can()` — is
 * the one that changes how an app should be written:
 *
 *   if (await toft.app.can("email")) { … } else { renderNotEnabledNotice(); }
 *
 * The answer is attached-or-not and never *why* not. The reason is an
 * administrator's business, and an app is not a principal that gets told about
 * the install's configuration.
 *
 * **Storage is resolved by name, never by path.** Mounted paths are declared so
 * an administrator can move a resource; an app that hard-codes `/data/reports`
 * breaks the first time one does. A miss names what actually *is* mounted,
 * because "no such resource" without the list sends somebody to their
 * administrator for a fact this process already has.
 */

import fs from "node:fs/promises";
import path from "node:path";

import { ToftError, transport } from "./transport.js";

const ENV_STORAGE = "TOFT_STORAGE";
const ENV_BASE_PATH = "BASE_PATH";

/** This app, as the platform sees it. */
export class App {
  #view = null;

  // Fetched once and cached for the life of the process: it is read on hot
  // paths and an administrator's change lands at the next deploy anyway.
  // `refresh()` re-reads it for a long-lived app that would rather notice
  // sooner.
  async #load() {
    return (this.#view ??= (await transport().get("/api/v1/app/self")) ?? {});
  }

  refresh() {
    this.#view = null;
    return this;
  }

  async id() {
    return (await this.#load()).id ?? "";
  }

  async slug() {
    return (await this.#load()).slug ?? "";
  }

  async name() {
    return (await this.#load()).name ?? "";
  }

  /** `shared` or `isolated`. */
  async mode() {
    return (await this.#load()).mode ?? "";
  }

  /**
   * Where this app is mounted, e.g. `/apps/reporter`.
   *
   * **Build every link and form action from this**, because a root-absolute URL
   * asks the platform rather than the app and 404s.
   *
   * The platform sends it; `BASE_PATH` in the environment is the fallback, and
   * it is a fallback rather than the source so that the accessor and the
   * variable cannot disagree — the variable is the value this same deploy
   * injected into this process, so it can only answer where an install older
   * than the field says nothing. Reporting `""` there would be worse than
   * useless: `""` is also the honest answer on an install that gives each app
   * its own origin, so nothing an app could check told the two apart.
   */
  async basePath() {
    return (await this.#load()).base_path ?? (process.env[ENV_BASE_PATH] ?? "").trim();
  }

  /**
   * The read-only policy block: the rules this app is judged by — version
   * retention, whether it may sleep and why. The same block `GET /apps/{id}`
   * returns, for the same reason it exists there: a platform decision about one
   * app is visible on that app.
   */
  async policy() {
    return (await this.#load()).policy ?? {};
  }

  /** Every capability an administrator attached, e.g. `email.send`. */
  async capabilities() {
    return (await this.#load()).capabilities ?? [];
  }

  /**
   * Whether this app holds a capability.
   *
   * Takes a full capability (`email.send`) or an integration name (`email`),
   * which is true when *any* of that integration's actions is attached. Both
   * spellings are accepted because both are the obvious thing to type, and
   * guessing wrong should not be a silent false.
   */
  async can(capability) {
    if (!capability) return false;
    const held = await this.capabilities();
    return held.includes(capability) || held.some((c) => c.startsWith(`${capability}.`));
  }
}

/** One durable resource mapped to this app. */
export class Mount {
  constructor(raw) {
    this.name = raw.name ?? "";
    this.kind = raw.kind ?? "";
    this.path = raw.path ?? "";
    this.access = raw.access ?? "read";
    // Which directory of the resource this mount is rooted at, and "" is the
    // whole resource. It is here because **two mounts of one resource share a
    // name**, and the name is how app code locates a resource — so this is what
    // tells them apart. Deliberately not part of `path`: where a mount appears
    // stays the administrator's choice.
    this.folder = raw.folder ?? "";
  }

  /** Whether this mount permits a write at all — every rung above `read`. */
  get writable() {
    return this.access !== "read" && this.access !== "";
  }

  /**
   * Copy a local file into this mount, and resolve to where it landed.
   *
   *     await toft.store("reports").put("nightly.parquet", "2026-08.parquet");
   *
   * Sugar over the mounted path and nothing more. An app that has the store
   * mapped already has the folder on its filesystem; this saves it from joining
   * paths. It deliberately does **not** call the platform's presign routes — an
   * SDK that could presign its way into a store would let a shared-mode app
   * write to durable storage, which ADR §12a refuses at deploy time on purpose.
   *
   * The access level is checked here only for a better message. **The mount is
   * the enforcement**: Mountpoint refuses an unsupported operation, so an app
   * that tries to replace an object it was granted `write` on fails at the first
   * wrong call rather than corrupting data slowly.
   */
  async put(source, name) {
    const targetName = name ?? path.basename(source);
    checkRelativeName(targetName);

    if (!this.writable) {
      throw new ToftError(
        `${JSON.stringify(this.name)} is mounted read-only, so this app cannot write to it. ` +
          `An administrator sets the access level where the resource is mapped.`,
      );
    }

    const destination = path.join(this.path, targetName);
    await fs.mkdir(path.dirname(destination), { recursive: true });
    await fs.copyFile(source, destination);
    return destination;
  }
}

/**
 * Refuse a name that would land outside the mount.
 *
 * The same discipline the platform applies to an uploaded object's name, for the
 * same reason: the name is the caller's and the location is not, so a traversal
 * segment here would write into the app's own filesystem — over its code, in the
 * worst case — rather than into the storage it was given.
 */
function checkRelativeName(name) {
  if (!name) throw new ToftError("a name is required");
  if (name.startsWith("/") || name.endsWith("/")) {
    throw new ToftError(`a name is relative to the mount, got ${JSON.stringify(name)}`);
  }
  if (name.includes("\\")) {
    throw new ToftError(`a name must use forward slashes, got ${JSON.stringify(name)}`);
  }
  for (const segment of name.split("/")) {
    if (segment === "" || segment === "." || segment === "..") {
      throw new ToftError(
        `a name must not contain ${JSON.stringify(segment)} segments, got ${JSON.stringify(name)}`,
      );
    }
  }
}

/**
 * Every durable resource mapped to this app, in the order the platform listed
 * them.
 *
 * That order is the platform's `ORDER BY m.path`, which is worth knowing only
 * because two mounts can share a name: it is stable, and it is not a ranking.
 * Ask for the one you mean with `{ folder }`.
 *
 * Read from the injected environment, which the platform keeps in step with
 * `GET /api/v1/app/storage` — same shape, so either source resolves to the same
 * answer.
 */
export function mounts() {
  const raw = process.env[ENV_STORAGE];
  if (!raw) return [];
  try {
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.map((m) => new Mount(m)) : [];
  } catch {
    return [];
  }
}

/**
 * Resolve one **store** by name, and — where it is mapped twice — by folder.
 *
 * A kind-checked alias of `storage`. Uploads are a store feature end to end —
 * the CLI command is `toft store`, the grant table refuses a volume — so a reader
 * never has to remember which kinds are in scope, because the name is the kind.
 *
 * `storage()` keeps answering for both kinds, because app code that has a volume
 * mounted has done nothing wrong.
 */
export function store(name, options) {
  const mount = storage(name, options);
  if (mount.kind && mount.kind !== "store") {
    throw new ToftError(
      `${JSON.stringify(name)} is a ${mount.kind}, not a store. A volume is a real ` +
        `filesystem — write to toft.storage(${JSON.stringify(name)}).path directly, with the ` +
        `ordinary file APIs.`,
    );
  }
  return mount;
}

/**
 * Resolve one resource by name, and — where it is mapped twice — by folder.
 *
 *     toft.storage("warehouse").path
 *     toft.storage("drop", { folder: "backup" }).path
 *
 * **A name is not always one mount.** A mapping's identity is `(resource, app,
 * folder)`, so an administrator can map one resource twice: the root read-only
 * and a folder at `write`, at two paths in one workload. Both arrive under the
 * same name, and `folder` is what tells them apart.
 *
 * So a name matching two mounts, with no folder named, **throws rather than
 * picking one**. Returning the first was the earlier behaviour and it made the
 * choice by the alphabetical order of paths an administrator chose — a silent
 * answer to a question the caller did not know they were asking.
 *
 * An absent `folder` means *do not care*, which is the ordinary case and is only
 * ambiguous when there is genuinely more than one. `folder: ""` names the root.
 */
export function storage(name, { folder } = {}) {
  const all = mounts();
  const named = all.filter((m) => m.name === name);

  if (folder !== undefined) {
    const exact = named.find((m) => m.folder === folder);
    if (exact) return exact;
    if (named.length) {
      throw new ToftError(
        `${JSON.stringify(name)} is mounted, but not at folder ${JSON.stringify(folder)}. ` +
          `Mounted here: ${describeFolders(named)}.`,
      );
    }
  } else if (named.length === 1) {
    return named[0];
  } else if (named.length > 1) {
    throw new ToftError(
      `${named.length} mounts are named ${JSON.stringify(name)} — ${describeFolders(named)}. ` +
        `Name one: toft.storage(${JSON.stringify(name)}, { folder: ` +
        `${JSON.stringify(named[0].folder)} }).`,
    );
  }

  const available = all.map((m) => m.name);
  throw new ToftError(
    available.length
      ? `no storage resource called ${JSON.stringify(name)} is mapped to this app. ` +
        `Mapped here: ${available.join(", ")}.`
      : `no storage resource called ${JSON.stringify(name)} is mapped to this app, and ` +
        `in fact none is. An administrator maps durable storage to an app under ` +
        `Admin → Storage.`,
  );
}

/**
 * The folders one name is mounted at, with the level of each.
 *
 * The level is in the message because it is usually the reason there are two: a
 * read corpus and a write drop are the shape this feature exists for, and a
 * caller who picked the wrong one wants to see that immediately.
 */
function describeFolders(named) {
  return named
    .map((m) => `folder ${JSON.stringify(m.folder)} at ${m.access || "read"}`)
    .join(", ");
}
