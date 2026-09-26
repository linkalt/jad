import {lookup} from "node:dns/promises";
import {request as httpRequest, type IncomingMessage} from "node:http";
import {request as httpsRequest} from "node:https";
import {Readable} from "node:stream";
import {createBrotliDecompress, createGunzip, createInflate} from "node:zlib";

import {convert} from "@xberg-io/html-to-markdown";

import type {NativeFetchToolSettings} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import {executionError, renderToolResult} from "../output.js";
import type {ToolDefinition, ToolExecutionContext} from "../types.js";
import {FetchStore, FetchStoreError, type FetchDocument} from "./fetch-store.js";

export const NATIVE_FETCH_ID = "native.fetch";
export const nativeFetchDefinition: ToolDefinition = {
  id: NATIVE_FETCH_ID,
  source: "native",
  name: "fetch",
  description: "Fetch a public text or HTML page; HTML is converted to Markdown and other text is returned as-is. Use cursor to continue.",
  inputSchema: {
    type: "object",
    properties: {
      url: {type: "string", minLength: 1, description: "Public http or https URL to fetch when cursor is absent or no longer available."},
      cursor: {type: "string", minLength: 1, description: "Cursor returned by a previous fetch call. A valid cursor takes precedence over url."},
      limit: {type: "integer", minimum: 1, description: "Optional character limit for this page."},
    },
    additionalProperties: false,
    anyOf: [
      {properties: {url: {}}, required: ["url"]},
      {properties: {cursor: {}}, required: ["cursor"]},
    ],
  },
};

interface FetchContext extends ToolExecutionContext {
  policy: NativeFetchToolSettings;
  store: FetchStore;
  fetchFn?: typeof globalThis.fetch;
}

export function normalizeFetchArguments(args: JsonRecord): JsonRecord {
  const normalized = {...args};
  for (const key of ["url", "cursor"] as const) {
    if (typeof normalized[key] === "string") normalized[key] = normalized[key].trim();
  }
  if (typeof normalized.limit === "number" && Number.isSafeInteger(normalized.limit)) {
    normalized.limit = Math.max(1, normalized.limit);
  }
  return normalized;
}

class FetchRequestError extends Error {
  public constructor(message: string, public readonly code: string) {
    super(message);
    this.name = "FetchRequestError";
  }
}

function privateAddress(address: string): boolean {
  const value = address.toLowerCase();
  if (value.includes(":")) {
    if (value.startsWith("::ffff:")) {
      const mapped = value.slice(7);
      if (mapped.includes(".")) return privateAddress(mapped);
      const hex = /^([0-9a-f]{1,4}):([0-9a-f]{1,4})$/.exec(mapped);
      if (hex) {
        const high = Number.parseInt(hex[1]!, 16);
        const low = Number.parseInt(hex[2]!, 16);
        return privateAddress(`${high >>> 8}.${high & 255}.${low >>> 8}.${low & 255}`);
      }
      return true;
    }
    return value === "::" || value === "::1" || value.startsWith("fc") || value.startsWith("fd") || /^fe[89ab]/.test(value) || value.startsWith("2001:db8:");
  }
  const parts = value.split(".").map(Number);
  if (parts.length !== 4 || parts.some((part) => !Number.isInteger(part) || part < 0 || part > 255)) return true;
  const first = parts[0]!;
  const second = parts[1]!;
  return first === 0 || first === 10 || first === 127 || first >= 224 || (first === 100 && second >= 64 && second <= 127) || (first === 169 && second === 254) || (first === 172 && second >= 16 && second <= 31) || (first === 192 && (second === 0 || second === 168)) || (first === 198 && (second === 18 || second === 19));
}

function hostnameAllowed(hostname: string, policy: NativeFetchToolSettings): void {
  const host = hostname.toLowerCase();
  if (policy.denied_hosts.includes(host)) throw new FetchRequestError("URL host is denied by fetch policy", "host_denied");
  if (policy.allowed_hosts.length > 0 && !policy.allowed_hosts.includes(host)) throw new FetchRequestError("URL host is not allowed by fetch policy", "host_denied");
}

interface ValidatedUrl {
  url: URL;
  address: string;
  family: 4 | 6;
}

async function validateUrl(value: string, policy: NativeFetchToolSettings, signal: AbortSignal): Promise<ValidatedUrl> {
  let url: URL;
  try { url = new URL(value); } catch { throw new FetchRequestError("url must be an absolute http or https URL", "invalid_url"); }
  if (url.protocol !== "http:" && url.protocol !== "https:") throw new FetchRequestError("url must use http or https", "invalid_url");
  if (url.username || url.password) throw new FetchRequestError("url must not include credentials", "invalid_url");
  const hostname = url.hostname.replace(/^\[|\]$/g, "");
  hostnameAllowed(hostname, policy);
  let addresses: Array<{address: string; family: number}>;
  if (signal.aborted) throw new FetchRequestError("fetch timed out or was cancelled", "timeout");
  let rejectAbort: (error: Error) => void = () => {};
  const aborted = new Promise<never>((_, reject) => { rejectAbort = reject; });
  const onAbort = () => rejectAbort(new FetchRequestError("fetch timed out or was cancelled", "timeout"));
  signal.addEventListener("abort", onAbort, {once: true});
  try {
    if (signal.aborted) onAbort();
    addresses = await Promise.race([lookup(hostname, {all: true, verbatim: true}), aborted]);
  } catch (error) {
    if (error instanceof FetchRequestError) throw error;
    throw new FetchRequestError("URL host could not be resolved", "unresolvable_host");
  } finally {
    signal.removeEventListener("abort", onAbort);
  }
  if (addresses.length === 0 || addresses.some(({address}) => privateAddress(address))) throw new FetchRequestError("URL resolves to a private or reserved address", "unsafe_url");
  const selected = addresses[0]!;
  if (selected.family !== 4 && selected.family !== 6) throw new FetchRequestError("URL host returned an invalid address", "unsafe_url");
  return {url, address: selected.address, family: selected.family};
}

/** Connect to the validated IP while preserving the URL hostname for Host and TLS SNI. */
export function pinnedFetch(target: ValidatedUrl, signal: AbortSignal): Promise<Response> {
  return new Promise((resolve, reject) => {
    const request = (target.url.protocol === "https:" ? httpsRequest : httpRequest)(target.url, {
      signal,
      headers: {accept: "text/*, application/*", "accept-encoding": "gzip, deflate, br"},
      lookup: (_hostname, options, callback) => {
        if (options.all) callback(null, [{address: target.address, family: target.family}]);
        else callback(null, target.address, target.family);
      },
    }, (incoming: IncomingMessage) => {
      const headers = new Headers();
      for (const [key, value] of Object.entries(incoming.headers)) {
        if (typeof value === "string") headers.set(key, value);
        else if (Array.isArray(value)) for (const entry of value) headers.append(key, entry);
      }
      const status = incoming.statusCode ?? 500;
      const noBody = status === 204 || status === 205 || status === 304;
      const encoding = headers.get("content-encoding")?.trim().toLowerCase();
      let body: Readable = incoming;
      if (!noBody && encoding && encoding !== "identity") {
        const decoder = encoding === "gzip" ? createGunzip()
          : encoding === "deflate" ? createInflate()
          : encoding === "br" ? createBrotliDecompress()
          : undefined;
        if (!decoder) {
          incoming.destroy();
          reject(new FetchRequestError("response content encoding is not supported", "unsupported_content_encoding"));
          return;
        }
        incoming.once("error", (error) => decoder.destroy(error));
        decoder.once("close", () => incoming.destroy());
        body = incoming.pipe(decoder);
        headers.delete("content-encoding");
        // The compressed wire length is not the limit for the decoded text.
        headers.delete("content-length");
      }
      try {
        resolve(new Response(noBody ? null : Readable.toWeb(body) as ReadableStream<Uint8Array>, {status, headers}));
        if (noBody) incoming.resume();
      } catch (error) {
        incoming.destroy();
        reject(error);
      }
    });
    request.once("error", reject);
    request.end();
  });
}

async function responseText(response: Response, maximum: number, signal: AbortSignal): Promise<string> {
  const declared = response.headers.get("content-length");
  if (declared && Number(declared) > maximum) {
    void response.body?.cancel().catch(() => undefined);
    throw new FetchRequestError("response exceeds configured byte limit", "response_too_large");
  }
  if (!response.body) return "";
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let bytes = 0;
  let rejectAbort: (error: Error) => void = () => {};
  const aborted = new Promise<never>((_, reject) => { rejectAbort = reject; });
  const onAbort = () => {
    rejectAbort(new FetchRequestError("fetch timed out or was cancelled", "timeout"));
    void reader.cancel().catch(() => undefined);
  };
  signal.addEventListener("abort", onAbort, {once: true});
  if (signal.aborted) onAbort();
  try {
    while (true) {
      const {done, value} = await Promise.race([reader.read(), aborted]);
      if (done) break;
      bytes += value.byteLength;
      if (bytes > maximum) {
        void reader.cancel().catch(() => undefined);
        throw new FetchRequestError("response exceeds configured byte limit", "response_too_large");
      }
      chunks.push(value);
    }
  } finally {
    signal.removeEventListener("abort", onAbort);
  }
  const body = new Uint8Array(bytes);
  let offset = 0;
  for (const chunk of chunks) { body.set(chunk, offset); offset += chunk.byteLength; }
  return new TextDecoder().decode(body);
}

function title(html: string): string | undefined {
  const match = /<title[^>]*>([\s\S]*?)<\/title>/i.exec(html);
  const value = match?.[1]?.replace(/<[^>]+>/g, " ").replace(/\s+/g, " ").trim();
  return value || undefined;
}

const KNOWN_TEXT_APPLICATION_TYPES = new Set([
  "application/markdown",
  "application/x-markdown",
  "application/json",
  "application/xml",
  "application/yaml",
  "application/x-yaml",
  "application/toml",
  "application/x-toml",
  "application/javascript",
  "application/x-javascript",
  "application/ecmascript",
  "application/sql",
  "application/graphql",
  "application/ndjson",
  "application/x-ndjson",
  "application/rtf",
  "application/x-www-form-urlencoded",
]);

function isSupportedTextContentType(contentType: string): boolean {
  return contentType.startsWith("text/") ||
    KNOWN_TEXT_APPLICATION_TYPES.has(contentType) ||
    /^application\/[^/]+\+(?:json|xml)$/.test(contentType);
}

function isHtmlContentType(contentType: string): boolean {
  return contentType === "text/html" || contentType === "application/xhtml+xml";
}

async function retrieve(urlValue: string, context: FetchContext): Promise<Omit<FetchDocument, "id" | "fetchedAt">> {
  const controller = new AbortController();
  const onRunAbort = () => controller.abort(context.signal?.reason);
  context.signal?.addEventListener("abort", onRunAbort, {once: true});
  if (context.signal?.aborted) onRunAbort();
  // One wall-clock deadline covers DNS, redirects, response headers and body.
  const timeout = setTimeout(() => controller.abort(new Error("fetch deadline exceeded")), context.policy.timeout_ms);
  let nextUrl = urlValue;
  try {
    for (let redirects = 0; ; redirects += 1) {
      const target = await validateUrl(nextUrl, context.policy, controller.signal);
      if (controller.signal.aborted) throw new FetchRequestError("fetch timed out or was cancelled", "timeout");
      const response = context.fetchFn
        ? await context.fetchFn(target.url, {redirect: "manual", signal: controller.signal, headers: {accept: "text/*, application/*"}})
        : await pinnedFetch(target, controller.signal);
      if (controller.signal.aborted) {
        if (!context.fetchFn) void response.body?.cancel().catch(() => undefined);
        throw new FetchRequestError("fetch timed out or was cancelled", "timeout");
      }
      if (response.status >= 300 && response.status < 400) {
        if (redirects >= context.policy.max_redirects) throw new FetchRequestError("fetch exceeded configured redirect limit", "too_many_redirects");
        const location = response.headers.get("location");
        if (!location) throw new FetchRequestError("redirect response has no location", "invalid_redirect");
        if (!context.fetchFn) void response.body?.cancel().catch(() => undefined);
        nextUrl = new URL(location, target.url).toString();
        continue;
      }
      if (!response.ok) {
        if (!context.fetchFn) void response.body?.cancel().catch(() => undefined);
        throw new FetchRequestError(`web request returned HTTP ${response.status}`, "http_error");
      }
      const contentType = response.headers.get("content-type")?.split(";", 1)[0]?.trim().toLowerCase() ?? "";
      if (!isSupportedTextContentType(contentType)) {
        if (!context.fetchFn) void response.body?.cancel().catch(() => undefined);
        throw new FetchRequestError("response content type is not a supported text format", "unsupported_content_type");
      }
      const body = await responseText(response, context.policy.max_response_bytes, controller.signal);
      const isHtml = isHtmlContentType(contentType);
      const markdown = isHtml
        ? convert(body, {
            extractMetadata: context.policy.include_metadata,
            skipImages: !context.policy.include_images,
            extractImages: false,
            includeDocumentStructure: false,
          }).content ?? ""
        : body;
      if (markdown.length > context.policy.max_document_chars) throw new FetchRequestError("document exceeds configured character limit", "document_too_large");
      const pageTitle = isHtml ? title(body) : undefined;
      return {url: urlValue, finalUrl: target.url.toString(), ...(pageTitle ? {title: pageTitle} : {}), contentType, status: response.status, markdown};
    }
  } catch (error) {
    if (error instanceof FetchRequestError) throw error;
    if (controller.signal.aborted || (error as {name?: string}).name === "AbortError") {
      throw new FetchRequestError("fetch timed out or was cancelled", "timeout");
    }
    throw new FetchRequestError("web request failed", "request_failed");
  } finally {
    clearTimeout(timeout);
    context.signal?.removeEventListener("abort", onRunAbort);
  }
}

function error(context: FetchContext, code: string, message: string, status: "invalid_input" | "error" = "error"): string {
  return executionError(context.callId, status, code, message);
}

function render(context: FetchContext, page: {document: FetchDocument; content: string; nextCursor?: string}): string {
  const {document} = page;
  return renderToolResult({
    id: context.callId,
    status: "ok",
    summary: page.nextCursor ? "Document fetched; more content is available" : "Document fetched",
    information: [
      ["url", document.url], ["final_url", document.finalUrl], ["status", document.status], ["content_type", document.contentType],
      ...(document.title ? [["title", document.title] as [string, unknown]] : []),
      ...(page.nextCursor ? [["next_cursor", page.nextCursor] as [string, unknown]] : []),
    ],
    content: {encoding: "utf8", text: page.content},
  });
}

export async function executeNativeFetch(args: JsonRecord, context: FetchContext): Promise<string> {
  const keys = Object.keys(args);
  if (keys.some((key) => !["url", "cursor", "limit"].includes(key))) return error(context, "invalid_input", "Fetch accepts url, cursor, and limit", "invalid_input");
  const url = typeof args.url === "string" ? args.url.trim() : undefined;
  const cursor = typeof args.cursor === "string" ? args.cursor : undefined;
  if (!url && !cursor) return error(context, "invalid_input", "Fetch requires url or cursor", "invalid_input");
  const requested = typeof args.limit === "number" && Number.isSafeInteger(args.limit) ? args.limit : context.policy.default_limit;
  if (requested < 1) return error(context, "invalid_limit", "limit must be a positive integer", "invalid_input");
  const limit = Math.min(requested, context.policy.max_limit);
  try {
    if (cursor) {
      try {
        return render(context, await context.store.page(cursor, limit));
      } catch (cause) {
        if (!(cause instanceof FetchStoreError) || !url) throw cause;
      }
    }
    return render(context, await context.store.createPage(await retrieve(url!, context), limit, context.policy));
  } catch (cause) {
    if (cause instanceof FetchRequestError) return error(context, cause.code, cause.message, cause.code === "invalid_url" ? "invalid_input" : "error");
    if (cause instanceof FetchStoreError) return error(context, "fetch_storage_error", cause.message);
    return error(context, "fetch_failed", cause instanceof Error ? cause.message : String(cause));
  }
}
