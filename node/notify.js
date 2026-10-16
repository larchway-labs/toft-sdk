/**
 * Sending messages and files *as this app*, through whatever an administrator
 * has connected.
 *
 * The platform holds the credential. This app never sees the workspace bot token
 * or the relay password, never chooses which Slack channel it posts to, and
 * never chooses the address its mail comes from. It supplies the message; the
 * platform supplies the identity. That is the whole shape of the feature, and it
 * is what makes an administrator's "detach Slack from that app" a row they
 * delete rather than a token they rotate across every app on the install.
 *
 *     await toft.notify.send("slack", "Ingest finished: 1,204 rows in 42s");
 *     await toft.notify.deliver("sharepoint", "/tmp/recon.xlsx", { name: "Week 32.xlsx" });
 *     await toft.notify.email({ to: "finance@example.com", subject: "…", text: "…" });
 *
 * The three older calls — `slack()`, `slackFile()` and `email()` — keep working
 * unchanged, permanently. They are the general ones with the integration
 * supplied, and an app written against them keeps running on every release.
 *
 * Three properties to write app code against.
 *
 * **Nothing here blocks on delivery.** Every send resolves as soon as the
 * platform has accepted and recorded it, with a `Send` whose `id` names the
 * record. Delivery happens afterwards. Resolving without throwing means
 * *accepted*, not *delivered* — deliberately, because a report generator that
 * fell over because Slack was having an afternoon would be a worse outcome than
 * a report that arrives late.
 *
 * **The record is what you rely on, not the message.** `status()` reads back one
 * send by its id. Owners see the same thing on the app's Integrations tab;
 * administrators see every app's on the install's integration log.
 *
 * **Where it goes is not yours to choose.** There is no `channel` argument, and
 * `from` / `replyTo` are not fields. Sending one is a refusal, not a silently
 * ignored field — a dropped field is a caller who believes they chose. An owner
 * who wants a different Slack channel or a team reply-to sets it on the app,
 * where it is stored, audited, and visible to everyone who can read the app.
 *
 * This mirrors `toft.notifier` in the Python SDK call for call. The two
 * have to behave identically, or an app written in one observes a platform the
 * other cannot.
 */

import { basename } from "node:path";
import { readFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";

import { ToftError, transport } from "./transport.js";

const SLACK_PATH = "/api/v1/app/notify/slack";
const SLACK_FILE_PATH = "/api/v1/app/notify/slack/file";
const EMAIL_PATH = "/api/v1/app/notify/email";
const SENDS_PATH = "/api/v1/app/sends";

/** The general form: one route per shape, with the integration in the path. */
const notifyPath = (kind) => `/api/v1/app/notify/${encodeURIComponent(kind)}`;
const deliverPath = (kind) => `/api/v1/app/deliver/${encodeURIComponent(kind)}`;

/** Statuses that will not change again. */
const TERMINAL_STATUSES = new Set(["sent", "failed", "blocked", "abandoned"]);

/** One accepted send, and the platform's record of it. */
export class Send {
  constructor(raw) {
    const data = raw && typeof raw === "object" ? raw : {};
    this.id = String(data.send_id ?? data.id ?? "");
    this.status = String(data.status ?? "queued");
    /** Which integration carried it. `integration` is the older name for the
     * same fact and is kept forever — an app written against one release keeps
     * running on the next. */
    this.kind = String(data.kind ?? data.integration ?? "");
    this.integration = this.kind;
    this.action = String(data.action ?? "");
    this.error = String(data.error ?? "");
    this.raw = { ...data };
  }

  get done() {
    return TERMINAL_STATUSES.has(this.status);
  }

  get delivered() {
    return this.status === "sent";
  }

  /**
   * True when an administrator turned the capability off after accepting.
   *
   * A distinct outcome from a failure and worth distinguishing in an app's own
   * logs: nothing is wrong with the message, and retrying will not help until
   * somebody re-attaches the integration.
   */
  get blocked() {
    return this.status === "blocked";
  }
}

/**
 * One file in a multipart request.
 *
 * The filename is reduced to a basename here as well as on the platform. Not
 * because this side is trusted — it is not, and the platform sanitizes again —
 * but because a caller passing "/tmp/x/report.xlsx" means the file, and silently
 * mailing a filename with somebody's directory layout in it is a small
 * disclosure nobody chose.
 */
class Part {
  constructor(field, filename, content, contentType = "") {
    this.field = field;
    this.filename = basename(String(filename)) || "attachment";
    this.content = content;
    this.contentType = contentType || guessType(this.filename);
  }

  /**
   * Accept the shapes a caller naturally reaches for: a path, a `[name, bytes]`
   * pair, or an object with `name` and `content`. Anything else is a clear
   * error rather than a stringified object arriving as an attachment called
   * "[object Object]".
   */
  static async coerce(field, value) {
    if (value instanceof Part) return value;
    if (typeof value === "string") {
      let content;
      try {
        content = await readFile(value);
      } catch (err) {
        throw new ToftError(`cannot read ${value}: ${err.message}`);
      }
      return new Part(field, value, content);
    }
    if (Array.isArray(value) && value.length === 2) {
      return new Part(field, value[0], toBytes(value[1]));
    }
    if (value && typeof value === "object") {
      return new Part(
        field,
        value.name ?? "attachment",
        toBytes(value.content ?? new Uint8Array()),
        value.type ?? "",
      );
    }
    throw new ToftError(
      "an attachment must be a path, a [name, bytes] pair, or {name, content}",
    );
  }
}

const TYPES = {
  ".csv": "text/csv",
  ".html": "text/html",
  ".json": "application/json",
  ".pdf": "application/pdf",
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".jpeg": "image/jpeg",
  ".txt": "text/plain",
  ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
  ".zip": "application/zip",
};

function guessType(filename) {
  const dot = filename.lastIndexOf(".");
  return (dot >= 0 && TYPES[filename.slice(dot).toLowerCase()]) || "application/octet-stream";
}

function toBytes(value) {
  if (typeof value === "string") return new TextEncoder().encode(value);
  return value;
}

function requireText(value, what) {
  if (typeof value !== "string" || !value.trim()) {
    throw new ToftError(`${what} needs some text`);
  }
  return value;
}

/** Accept one address or many, because both are natural to write. */
function addresses(value, field) {
  const items = typeof value === "string" ? [value] : Array.from(value ?? []);
  const out = items.map((v) => String(v).trim()).filter(Boolean);
  if (items.length && !out.length) {
    throw new ToftError(`'${field}' held only blank addresses`);
  }
  return out;
}

/**
 * A caller's key, or a fresh one for this call.
 *
 * Generated once here rather than per HTTP attempt, which is the entire point:
 * the transport's single retry presents the same key, so a request that was in
 * fact delivered before the response was lost does not send twice.
 */
function defaultKey(key) {
  return key || randomUUID().replace(/-/g, "");
}

/** Messages and files, sent as this app. */
export class Notify {
  /**
   * Send one message through the named integration.
   *
   * The general form, and the one to write new code against: `send("slack",
   * …)`, `send("teams", …)`, `send("webhook", …)`. Which integrations this app
   * actually has is `toft.app.can("slack.post")` — ask at startup and render
   * "ask an administrator" on a screen, rather than discovering it at 2am.
   *
   * Where it goes is not an argument and never will be. The app's own people
   * choose the destination from what the connection an administrator made can
   * reach; sending a `channel`, `folder`, `url` or `routing_key` field is a
   * refusal rather than a silently ignored field, because a dropped field is a
   * caller who believes they chose.
   *
   * No idempotency key by default — a duplicate chat message is cheap, and a
   * key would be quietly deduplicating an app that meant to post twice. Pass
   * one for a message whose duplicate would matter.
   */
  async send(kind, text, { title, severity, dedupKey, action, idempotencyKey } = {}) {
    const name = requireText(kind, "integration");
    if (name === "email") {
      throw new ToftError(
        "email takes recipients and a subject — use toft.notify.email({to, subject, text})",
      );
    }
    const body = { text: requireText(text, name) };
    if (title) body.title = title;
    if (severity) body.severity = severity;
    if (dedupKey) body.dedup_key = dedupKey;
    // Only where a kind has more than one thing to do with a message.
    // PagerDuty's trigger and resolve are the pair that exists.
    if (action) body.action = action;
    return new Send(await transport().postJson(notifyPath(name), body, { idempotencyKey }));
  }

  /**
   * Close an incident this app raised.
   *
   * `dedupKey` is the same key the trigger used. It is namespaced with this
   * app before it leaves, which is what makes "an app can only resolve what it
   * triggered" true by construction rather than by a check: the key another
   * app's incident is under is not one this app can name.
   *
   * An app that may page and may not un-page leaves somebody awake with no way
   * to be told it is over, which is why this is not an optional half.
   */
  async resolve(kind, dedupKey, { reason, idempotencyKey } = {}) {
    return this.send(kind, reason || "Resolved by the application.", {
      action: "resolve",
      dedupKey: requireText(dedupKey, "dedup key"),
      idempotencyKey,
    });
  }

  /**
   * Put one file through the named integration — the "post the report" call.
   *
   * `name` is the name the file arrives under, and it is a *name*: a value
   * containing a separator, a leading dot or a reserved device name is refused
   * rather than rewritten. A silently sanitised name is a file this app cannot
   * find again. Where it lands is the administrator's folder, not this app's.
   *
   * A file is never replaced. Every integration that writes files adds, and a
   * name that already exists is renamed by the far side — an app cannot
   * overwrite a file, one it did not write or one it did.
   *
   * Defaults to an idempotency key, unlike `send()` — re-uploading a 20 MiB
   * report because a response was slow is not cheap, in the destination or on
   * the wire.
   */
  async deliver(kind, file, { name, title, comment, idempotencyKey } = {}) {
    const which = requireText(kind, "integration");
    let part = await Part.coerce("file", file);
    if (name) part = new Part("file", name, part.content, part.contentType);

    const fields = {};
    if (name) fields.name = basename(String(name));
    if (title) fields.title = title;
    if (comment) fields.comment = comment;

    const raw = await transport().postMultipart(deliverPath(which), fields, [part], {
      idempotencyKey: defaultKey(idempotencyKey),
    });
    return new Send(raw);
  }

  /**
   * Post one message to this app's Slack channel — `send("slack", …)` under its
   * published name, kept working permanently.
   *
   * `text` is Slack mrkdwn. The platform appends a line naming this app and
   * linking to it, which app code cannot suppress: an app that could post
   * indistinguishably from the platform is an app that can impersonate it.
   *
   * Which channel is not an argument and never will be. The app's owners choose
   * it from the channels somebody invited the bot into, falling back to the
   * install's default until they do.
   *
   * No idempotency key by default — a duplicate Slack post is cheap, and a key
   * would be quietly deduplicating an app that meant to post twice. Pass one for
   * a message whose duplicate would matter.
   */
  async slack(text, { idempotencyKey } = {}) {
    const body = { text: requireText(text, "slack") };
    return new Send(await transport().postJson(SLACK_PATH, body, { idempotencyKey }));
  }

  /**
   * Upload one file to this app's Slack channel — `deliver("slack", …)` under
   * its published name, kept working permanently.
   *
   * Not a separate capability from posting text: an app that may post to a
   * channel may upload a file to it. Defaults to an idempotency key, unlike
   * `slack()` — re-uploading a 20 MiB report because a response was slow is not
   * cheap, in the channel or on the wire.
   */
  async slackFile(file, { title, comment, filename, idempotencyKey } = {}) {
    let part = await Part.coerce("file", file);
    if (filename) part = new Part("file", filename, part.content, part.contentType);

    const fields = {};
    if (title) fields.title = title;
    if (comment) fields.comment = comment;

    const raw = await transport().postMultipart(SLACK_FILE_PATH, fields, [part], {
      idempotencyKey: defaultKey(idempotencyKey),
    });
    return new Send(raw);
  }

  /**
   * Send one email as this app.
   *
   * Recipients are unrestricted — any address, whether or not they have a
   * Toft account, which is the point: the weekly report goes to the finance
   * team, and the finance team does not log in here. Every send is recorded in
   * full, recipients included, and that record is the only control an
   * administrator has over this. Write app code that would read well in an
   * incident review.
   *
   * `text` is required even when `html` is given. The plain-text half is where
   * the "an application sent this" line is unmissable, and an HTML-only message
   * has no fallback for a reader whose client will not render it. `html` is
   * sanitized by the platform: scripts, styles, frames, event handlers and
   * positioning are removed. Style the content, not the frame.
   *
   * There is no `from` and no `replyTo`. Mail goes out as
   * `"<App Name> (via Toft)"` from the install's sending address, and
   * replies go to the app's owners — or to the address an owner set on the app's
   * Integrations tab.
   *
   * **Idempotency.** A key is generated by default, once per call, and reused if
   * this SDK retries. That is what stops a slow response from turning into two
   * copies of the report in somebody's inbox. It does *not* deduplicate across
   * processes: a cron-driven sender that wants "at most one report per week
   * however many times I run" should pass its own stable key.
   */
  async email({ to, subject, text, html, cc, bcc, attachments, idempotencyKey } = {}) {
    const recipients = addresses(to, "to");
    if (!recipients.length) {
      throw new ToftError("email needs at least one recipient in 'to'");
    }
    const body = {
      to: recipients,
      subject: requireText(subject, "email subject"),
      text: requireText(text, "email text"),
    };
    if (html) body.html = html;
    if (cc) body.cc = addresses(cc, "cc");
    if (bcc) body.bcc = addresses(bcc, "bcc");

    const parts = [];
    for (const a of attachments ?? []) parts.push(await Part.coerce("attachment", a));
    const key = defaultKey(idempotencyKey);

    // Attachments travel as multipart with the message as one JSON field, never
    // base64 inside a JSON body — see AppTransport.postMultipart.
    const raw = parts.length
      ? await transport().postMultipart(
          EMAIL_PATH,
          { payload: JSON.stringify(body) },
          parts,
          { idempotencyKey: key },
        )
      : await transport().postJson(EMAIL_PATH, body, { idempotencyKey: key });
    return new Send(raw);
  }

  /**
   * Read back one send this app made.
   *
   * The only thing an app may read about its own sends, and deliberately so:
   * there is no listing. An app browsing the record would be learning which
   * addresses its administrator finds interesting, whereas reading back an id it
   * minted, about content it wrote, tells it nothing it did not know.
   */
  async status(send) {
    const id = send instanceof Send ? send.id : String(send ?? "");
    if (!id) throw new ToftError("status() needs the id of a send");
    return new Send(await transport().get(`${SENDS_PATH}/${id}`));
  }
}
