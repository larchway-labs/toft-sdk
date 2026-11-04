/**
 * A file connection's own access token, for Microsoft's or Google's SDK.
 *
 * Where an administrator has switched on *Let apps get access tokens* on the
 * connection attached to this app, the app calls Graph or Drive directly and
 * holds no secret:
 *
 *   const token = await toft.accessToken("sharepoint");
 *   await fetch(`https://graph.microsoft.com/v1.0/drives/${token.folder.driveId}` +
 *     `/items/${token.folder.itemId}/children`,
 *     { headers: { Authorization: `Bearer ${token.value}` } });
 *
 * or, with Microsoft's own library:
 *
 *   const client = Client.initWithMiddleware({
 *     authProvider: new TokenCredentialAuthenticationProvider(
 *       toft.azureCredential("sharepoint"), { scopes: [] }) });
 *
 * **The folder is a hint, not a fence.** The token reaches everything the
 * connection's grant reaches; `folder` only says where this app was meant to
 * work, so its code names no site.
 *
 * Cached per kind and fetched again five minutes before expiry, so calling it
 * before every request costs one request to Toft an hour.
 */

import { inspect } from "node:util";

import { noMoreArgs, refusal, transport } from "./transport.js";

/** The connection kinds a token is handed out for. */
export const TOKEN_KINDS = ["sharepoint", "onedrive", "gdrive"];
/** The ones Microsoft's libraries accept a credential for. */
export const AZURE_KINDS = ["sharepoint", "onedrive"];

/** How long before expiry, in milliseconds, a cached token is replaced. */
export const REFRESH_MARGIN_MS = 5 * 60 * 1000;

/** One access token and what it is for. */
export class Token {
  constructor(raw) {
    const data = raw ?? {};
    const folder = data.folder ?? {};
    /**
     * The bearer token. Never log it. Not enumerable, so `console.log(token)`
     * — the first thing anyone debugging a Graph call types — does not print
     * it into the app's log, as the other three SDKs' printed forms do not.
     */
    Object.defineProperty(this, "value", { value: data.access_token ?? "", enumerable: false });
    /** When the provider stops accepting it. */
    this.expiresAt = new Date(data.expires_at ?? Date.now());
    /** The API it is for. */
    this.resource = data.resource ?? "";
    /** Where this app was attached, in the provider's ids. A hint, not a fence. */
    this.folder = {
      driveId: folder.drive_id ?? "",
      itemId: folder.item_id ?? "",
      label: folder.label ?? "",
    };
  }

  toJSON() {
    // Never the value: a token in a log line is held by whoever reads the log.
    return { resource: this.resource, expiresAt: this.expiresAt.toISOString(), folder: this.folder };
  }

  /** What `console.log` and `util.inspect` print: the toJSON shape, no value. */
  [inspect.custom](depth, options, inspectFn = inspect) {
    return `Token ${inspectFn(this.toJSON(), options)}`;
  }
}

const cache = new Map();

/**
 * The attached `kind` connection's token: "sharepoint", "onedrive" or "gdrive".
 * Rejects with a `ToftError` carrying the platform's words when the connection
 * hands out no tokens, or the token would reach more than files.
 */
export async function accessToken(kind) {
  // It takes no options; one passed is refused rather than ignored, an empty
  // {} included, as every other call that takes none refuses one (BB-114).
  noMoreArgs("accessToken()", arguments, 1, 'toft.accessToken("sharepoint")');
  // An empty kind reached POST /api/v1/app/tokens/ and came back as
  // route_not_found, which names a path rather than the mistake.
  if (typeof kind !== "string" || !kind.trim()) {
    throw refusal("bad_request", `accessToken() needs a connection kind, got ${JSON.stringify(kind) ?? String(kind)}`,
      { hint: `one of: ${TOKEN_KINDS.join(", ")}` });
  }
  const cached = cache.get(kind);
  if (cached && cached.expiresAt.getTime() - Date.now() > REFRESH_MARGIN_MS) {
    return cached;
  }
  const token = new Token(await transport().postJson(`/api/v1/app/tokens/${encodeURIComponent(kind)}`));
  cache.set(kind, token);
  return token;
}

/**
 * `@azure/identity`'s `TokenCredential` by shape, with no import of it:
 * Microsoft's libraries call `getToken` and get Toft's. The scopes are ignored —
 * the token is for Graph with whatever the connection was granted.
 */
export function azureCredential(kind) {
  noMoreArgs("azureCredential()", arguments, 1, 'toft.azureCredential("sharepoint")');
  // Refused here, as Python's is, rather than on the first getToken deep
  // inside Microsoft's library: a Google Drive token is not one Graph accepts.
  if (!AZURE_KINDS.includes(kind)) {
    throw refusal("bad_request", `azureCredential() is for a Microsoft connection, got ${JSON.stringify(kind) ?? String(kind)}`,
      { hint: `one of: ${AZURE_KINDS.join(", ")}` + (kind === "gdrive" ? ' — for gdrive use toft.accessToken("gdrive")' : "") });
  }
  return {
    async getToken() {
      const token = await accessToken(kind);
      return { token: token.value, expiresOnTimestamp: token.expiresAt.getTime() };
    },
  };
}

/** Forget every cached token. For tests. */
export function resetTokenCache() {
  noMoreArgs("resetTokenCache()", arguments, 0, "resetTokenCache()");
  cache.clear();
}
