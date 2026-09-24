import {lookup} from "node:dns/promises";

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
    if (value.startsWith("::ffff:") && value.slice(7).includes(".")) return privateAddress(value.slice(7));
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

async function validateUrl(value: string, policy: NativeFetchToolSettings): Promise<URL> {
  let url: URL;
  try { url = new URL(value); } catch { throw new FetchRequestError("url must be an absolute http or https URL", "invalid_url"); }
  if (url.protocol !== "http:" && url.protocol !== "https:") throw new FetchRequestError("url must use http or https", "invalid_url");
  if (url.username || url.password) throw new FetchRequestError("url must not include credentials", "invalid_url");
  hostnameAllowed(url.hostname, policy);
  let addresses: Array<{address: string}>;
  try { addresses = await lookup(url.hostname, {all: true, verbatim: true}); } catch { throw new FetchRequestError("URL host could not be resolved", "unresolvable_host"); }
  if (addresses.length === 0 || addresses.some(({address}) => privateAddress(address))) throw new FetchRequestError("URL resolves to a private or reserved address", "unsafe_url");
  return url;
}

async function responseText(response: Response, maximum: number): Promise<string> {
  const declared = response.headers.get("content-length");
  if (declared && Number(declared) > maximum) throw new FetchRequestError("response exceeds configured byte limit", "response_too_large");
  if (!response.body) return "";
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let bytes = 0;
  while (true) {
    const {done, value} = await reader.read();
    if (done) break;
    bytes += value.byteLength;
    if (bytes > maximum) {
      await reader.cancel();
      throw new FetchRequestError("response exceeds configured byte limit", "response_too_large");
    }
    chunks.push(value);
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
  let url = await validateUrl(urlValue, context.policy);
  const fetchFn = context.fetchFn ?? globalThis.fetch;
  for (let redirects = 0; ; redirects += 1) {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), context.policy.timeout_ms);
    let response: Response;
    try {
      response = await fetchFn(url, {redirect: "manual", signal: controller.signal, headers: {accept: "text/*, application/*"}});
    } catch (error) {
      if ((error as {name?: string}).name === "AbortError") throw new FetchRequestError("fetch timed out", "timeout");
      throw new FetchRequestError("web request failed", "request_failed");
    } finally { clearTimeout(timeout); }
    if (response.status >= 300 && response.status < 400) {
      if (redirects >= context.policy.max_redirects) throw new FetchRequestError("fetch exceeded configured redirect limit", "too_many_redirects");
      const location = response.headers.get("location");
      if (!location) throw new FetchRequestError("redirect response has no location", "invalid_redirect");
      url = await validateUrl(new URL(location, url).toString(), context.policy);
      continue;
    }
    if (!response.ok) throw new FetchRequestError(`web request returned HTTP ${response.status}`, "http_error");
    const contentType = response.headers.get("content-type")?.split(";", 1)[0]?.trim().toLowerCase() ?? "";
    if (!isSupportedTextContentType(contentType)) {
      throw new FetchRequestError("response content type is not a supported text format", "unsupported_content_type");
    }
    const body = await responseText(response, context.policy.max_response_bytes);
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
    return {url: urlValue, finalUrl: url.toString(), ...(pageTitle ? {title: pageTitle} : {}), contentType, status: response.status, markdown};
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
