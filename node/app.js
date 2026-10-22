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

import { createHash } from "node:crypto";
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
    // The path as the platform described it, and "" for a store that is not
    // mounted here. Read through `path`, which refuses rather than handing back
    // "" — `path.join("", name)` is the working directory, and writing there is
    // writing into the app's own tree.
    this.mountPath = raw.path ?? "";
    this.access = raw.access ?? "read";
    // Which directory of the resource this mount is rooted at, and "" is the
    // whole resource. It is here because **two mounts of one resource share a
    // name**, and the name is how app code locates a resource — so this is what
    // tells them apart. Deliberately not part of `path`: where a mount appears
    // stays the administrator's choice.
    this.folder = raw.folder ?? "";
  }

  /** Whether this resource is on this workload's filesystem at all. */
  get mounted() {
    return this.mountPath !== "";
  }

  get path() {
    if (!this.mounted) {
      throw refused("not_mounted",
        `${JSON.stringify(this.name)} is not mounted here — on this install the app reaches it ` +
          `through the platform: use list(), upload(), download() or url()`,
      );
    }
    return this.mountPath;
  }

  /** Whether this mount permits a write at all — every rung above `read`. */
  get writable() {
    return this.access !== "read" && this.access !== "";
  }

  /**
   * The same fact under the name the other three SDKs and the skill use —
   * Python's `read_only`, Go's `ReadOnly()`. Without it `m.readOnly` is
   * `undefined`, which renders as a blank and reads as falsy: a read-only
   * mount reported as writable by every app that ported the spelling.
   */
  get readOnly() {
    return !this.writable;
  }

  /**
   * Copy a local file into this mount, and resolve to where it landed.
   *
   *     await toft.store("reports").put("nightly.parquet", "2026-08.parquet");
   *
   * Sugar over the mounted path and nothing more. An app that has the store
   * mounted already has the folder on its filesystem; this saves it from joining
   * paths, and calls nothing. Where the store is not mounted — on an install with
   * no cluster — this refuses, and `upload()` reaches it through the platform
   * instead, at the same level, under the same mapping.
   *
   * The access level is checked here only for a better message. **The mount is
   * the enforcement**: Mountpoint refuses an unsupported operation, so an app
   * that tries to replace an object it was granted `write` on fails at the first
   * wrong call rather than corrupting data slowly.
   */
  async put(source, name) {
    const targetName = name ?? path.basename(source);
    checkRelativeName(targetName);

    if (!this.mounted) {
      throw refused("not_mounted",
        `${JSON.stringify(this.name)} is not mounted here, so put() has nowhere to copy to — ` +
          `use upload(), which reaches it through the platform`,
      );
    }
    if (!this.writable) {
      throw refused("insufficient_access",
        `${JSON.stringify(this.name)} is mounted read-only, so this app cannot write to it. ` +
          `An administrator sets the access level where the resource is mapped.`,
      );
    }

    const destination = path.join(this.path, targetName);
    await fs.mkdir(path.dirname(destination), { recursive: true });
    await fs.copyFile(source, destination);
    return destination;
  }

  // --- Over the platform's signed URLs, in both modes ---
  //
  // These reach a store through /api/v1/app/store whether or not it is
  // mounted, so an app written against them moves between execution modes
  // unchanged. The platform signs and never sees the bytes.

  /** One page of what is under `path`; names are relative to it. */
  async list(listPath = "", { cursor = "" } = {}) {
    if (listPath) checkRelativeName(listPath.replace(/\/$/, ""));
    const page =
      (await transport().get(
        this.#route("/api/v1/app/store/{slug}/objects", "", { path: listPath, cursor }),
      )) ?? {};
    const objects = page.objects ?? [];
    // Iterable over its objects, as Python's Listing is: `for (const o of
    // await s.list())` reads the same in both, and `length` is the page's.
    return {
      objects,
      prefixes: page.prefixes ?? [],
      nextCursor: page.next_cursor ?? "",
      access: page.access ?? "",
      length: objects.length,
      [Symbol.iterator]() {
        return objects[Symbol.iterator]();
      },
    };
  }

  /**
   * Upload a local file as `name` (its own name by default). At `write` this
   * creates new objects only, and an existing name is a refusal that says so.
   */
  async upload(source, name) {
    const target = name || path.basename(source);
    checkRelativeName(target);
    const data = await fs.readFile(source);
    const signed = await transport().postJson(this.#route("/api/v1/app/store/{slug}/objects"), {
      name: target,
      size: data.length,
      sha256: createHash("sha256").update(data).digest("hex"),
      md5: createHash("md5").update(data).digest("hex"),
    });
    try {
      await sendSigned(signed, data);
    } catch (err) {
      if (err instanceof ToftError && err.status === 412) {
        throw refused("already_exists",
          `${JSON.stringify(target)} already exists in ${JSON.stringify(this.name)}, and this ` +
            `app's access (${this.access}) creates new objects only`,
          { status: 412 },
        );
      }
      throw err;
    }
    return target;
  }

  /** Fetch one object's bytes, as a Buffer. */
  async download(name) {
    return sendSigned(await this.#signGet(name, false));
  }

  /**
   * A signed GET for one object, to hand to a browser. It lives fifteen
   * minutes, and anyone holding it reads that object until then — the point for
   * a download link, and the reason not to log one.
   */
  async url(name, { attachment = false } = {}) {
    return (await this.#signGet(name, attachment)).url;
  }

  /** Delete one object. Needs `full`. */
  async remove(name) {
    checkRelativeName(name);
    await sendSigned(await transport().del(this.#route("/api/v1/app/store/{slug}/objects/{name}", name)));
  }

  async #signGet(name, attachment) {
    checkRelativeName(name);
    const query = attachment ? { disposition: "attachment" } : {};
    return (
      (await transport().get(this.#route("/api/v1/app/store/{slug}/objects/{name}", name, query))) ?? {}
    );
  }

  // `route` is the platform's route, spelled whole so a reader — and the SDK
  // contract test — sees which one each call reaches. The folder always
  // travels, "" included: it names *this* mapping, so a resource mapped twice
  // is never a question the platform has to ask back.
  #route(route, name = "", query = {}) {
    if (this.kind && this.kind !== "store") {
      throw refused("not_a_store",
        `${JSON.stringify(this.name)} is a ${this.kind}, which is reached at its path — only ` +
          `an object store is reached over signed URLs`,
      );
    }
    const params = new URLSearchParams({ folder: this.folder });
    for (const [k, v] of Object.entries(query)) if (v) params.set(k, v);
    const filled = route
      .replace("{slug}", encodeURIComponent(this.name))
      .replace("{name}", encodeName(name));
    return `${filled}?${params}`;
  }
}

/**
 * A refusal this SDK makes itself, carrying the platform's envelope. None of
 * these is helped by trying again — a mapping, a name or a level is wrong — and
 * without a body the status rule would call a statusless error retryable, the
 * answer for a network failure. So each says so, with a code to branch on.
 */
function refused(code, message, options = {}) {
  return new ToftError(message, { ...options, body: { code, retryable: false } });
}

function encodeName(name) {
  return name.split("/").map(encodeURIComponent).join("/");
}

/**
 * Carry out a signed request against the object store: the signed headers
 * verbatim, and no Toft header at all — the URL is the whole authorization.
 */
async function sendSigned(signed, body) {
  if (!signed?.url) throw new ToftError("the platform returned no signed URL");
  let resp;
  try {
    resp = await fetch(signed.url, {
      method: signed.method || "GET",
      headers: signed.headers ?? {},
      body,
    });
  } catch (err) {
    throw new ToftError(`cannot reach the object store: ${err.message}`);
  }
  if (!resp.ok) {
    throw new ToftError(`the object store refused the request: ${resp.status} ${resp.statusText}`, {
      status: resp.status,
    });
  }
  return Buffer.from(await resp.arrayBuffer());
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
  if (!name) throw refused("invalid_name", "a name is required");
  if (name.startsWith("/") || name.endsWith("/")) {
    throw refused("invalid_name", `a name is relative to the mount, got ${JSON.stringify(name)}`);
  }
  if (name.includes("\\")) {
    throw refused("invalid_name", `a name must use forward slashes, got ${JSON.stringify(name)}`);
  }
  for (const segment of name.split("/")) {
    if (segment === "" || segment === "." || segment === "..") {
      throw refused("invalid_name",
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
    throw refused("not_a_store",
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
  // Before the mounts are read, as the other three SDKs do: no name is the
  // caller's mistake, not a resource that is missing.
  if (!name) {
    throw refused("invalid_name", "storage() needs the name an administrator gave the resource");
  }
  const all = mounts();
  const named = all.filter((m) => m.name === name);

  if (folder !== undefined) {
    const exact = named.find((m) => m.folder === folder);
    if (exact) return exact;
    if (named.length) {
      throw refused("not_mapped",
        `${JSON.stringify(name)} is mounted, but not at folder ${JSON.stringify(folder)}. ` +
          `Mounted here: ${describeFolders(named)}.`,
      );
    }
  } else if (named.length === 1) {
    return named[0];
  } else if (named.length > 1) {
    throw refused("ambiguous_mount",
      `${named.length} mounts are named ${JSON.stringify(name)} — ${describeFolders(named)}. ` +
        `Name one: toft.storage(${JSON.stringify(name)}, { folder: ` +
        `${JSON.stringify(named[0].folder)} }).`,
    );
  }

  const available = all.map((m) => m.name);
  throw refused("not_mapped",
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
