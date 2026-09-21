import { mkdir, readdir, writeFile } from "node:fs/promises";
import { join } from "node:path";

import type { HttpDebugSettings } from "./config.js";
import type { JsonRecord } from "./persistence.js";

const REDACTED = "[REDACTED]";
const SENSITIVE_HEADER = /(?:authorization|api[-_]?key|cookie|set-cookie|proxy-authorization)/i;

type CapturedBody = {
  body?: string;
  body_truncated?: boolean;
  body_unavailable?: boolean;
};

type RequestInfo = string | URL | Request;
type Fetch = (input: RequestInfo, init?: RequestInit) => Promise<Response>;

function isRequest(value: RequestInfo): value is Request {
  return typeof Request !== "undefined" && value instanceof Request;
}

function requestUrl(input: RequestInfo): string {
  if (isRequest(input)) {
    return input.url;
  }
  return String(input);
}

function requestMethod(input: RequestInfo, init: RequestInit | undefined): string {
  if (typeof init?.method === "string") {
    return init.method.toUpperCase();
  }
  return isRequest(input) ? input.method.toUpperCase() : "GET";
}

function requestHeaders(input: RequestInfo, init: RequestInit | undefined): Record<string, string> {
  const headers = new Headers(isRequest(input) ? input.headers : undefined);
  if (init?.headers !== undefined) {
    new Headers(init.headers).forEach((value, name) => headers.set(name, value));
  }

  const result: Record<string, string> = {};
  headers.forEach((value, name) => {
    result[name] = SENSITIVE_HEADER.test(name) ? REDACTED : value;
  });
  return result;
}

function truncateText(text: string, maxBytes: number): CapturedBody {
  const bytes = Buffer.from(text, "utf8");
  if (bytes.length <= maxBytes) {
    return { body: text, body_truncated: false };
  }
  return {
    body: bytes.subarray(0, maxBytes).toString("utf8"),
    body_truncated: true,
  };
}

async function bodyText(value: unknown): Promise<string | undefined> {
  if (typeof value === "string") {
    return value;
  }
  if (value instanceof URLSearchParams) {
    return value.toString();
  }
  if (typeof Blob !== "undefined" && value instanceof Blob) {
    return value.text();
  }
  if (value instanceof Uint8Array) {
    return Buffer.from(value).toString("utf8");
  }
  if (value instanceof ArrayBuffer) {
    return Buffer.from(value).toString("utf8");
  }
  if (ArrayBuffer.isView(value)) {
    return Buffer.from(value.buffer, value.byteOffset, value.byteLength).toString("utf8");
  }
  return undefined;
}

async function captureRequestBody(
  input: RequestInfo,
  init: RequestInit | undefined,
  maxBytes: number,
): Promise<CapturedBody> {
  if (init?.body !== undefined && init.body !== null) {
    const text = await bodyText(init.body);
    return text === undefined
      ? { body_unavailable: true }
      : truncateText(text, maxBytes);
  }
  if (isRequest(input) && input.body !== null) {
    return truncateText(await input.clone().text(), maxBytes);
  }
  return { body: "", body_truncated: false };
}

async function captureResponseBody(
  response: Response,
  maxBytes: number,
): Promise<CapturedBody> {
  const clone = response.clone();
  if (!clone.body) {
    return { body_unavailable: true };
  }

  const reader = clone.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  let truncated = false;
  try {
    while (total < maxBytes) {
      const result = await reader.read();
      if (result.done) {
        break;
      }
      const remaining = maxBytes - total;
      if (result.value.byteLength > remaining) {
        chunks.push(result.value.subarray(0, remaining));
        total = maxBytes;
        truncated = true;
        break;
      }
      chunks.push(result.value);
      total += result.value.byteLength;
    }
    if (total >= maxBytes && !truncated) {
      truncated = true;
    }
  } finally {
    void reader.cancel().catch(() => undefined);
  }

  return {
    body: Buffer.concat(chunks.map((chunk) => Buffer.from(chunk))).toString("utf8"),
    body_truncated: truncated,
  };
}

class HttpDebugLogger {
  private nextRequestId = 1;
  private pending: Promise<void> = Promise.resolve();
  private readonly ready: Promise<void>;

  public constructor(private readonly directory: string) {
    this.ready = this.initialize();
  }

  public async reserveRequestId(): Promise<number> {
    await this.ready;
    const requestId = this.nextRequestId;
    this.nextRequestId += 1;
    return requestId;
  }

  public log(fileName: string, record: JsonRecord): void {
    const path = join(this.directory, fileName);
    const contents = `${JSON.stringify(record)}\n`;
    this.pending = this.pending
      .then(async () => {
        await this.ready;
        await writeFile(path, contents, "utf8");
      })
      .catch((error: unknown) => {
        console.error(`Unable to write HTTP debug file ${path}:`, error);
      });
  }

  private async initialize(): Promise<void> {
    try {
      await mkdir(this.directory, { recursive: true });
      const entries = await readdir(this.directory, { withFileTypes: true });
      for (const entry of entries) {
        if (!entry.isFile()) {
          continue;
        }
        const match = /^(?:request|response)(\d+)$/.exec(entry.name);
        if (match) {
          this.nextRequestId = Math.max(this.nextRequestId, Number(match[1]) + 1);
        }
      }
    } catch (error) {
      console.error(`Unable to initialize HTTP debug directory ${this.directory}:`, error);
    }
  }
}

function responseHeaders(response: Response): Record<string, string> {
  const headers: Record<string, string> = {};
  response.headers.forEach((value, name) => {
    headers[name] = value;
  });
  return headers;
}

/**
 * Creates an OpenAI-compatible native fetch wrapper. Debug logging is
 * deliberately fire-and-forget so a diagnostic filesystem failure cannot alter
 * provider request semantics or interrupt a streaming response.
 */
export function createHttpDebugFetch(
  sessionDir: string,
  settings: HttpDebugSettings,
  nativeFetch: Fetch = globalThis.fetch.bind(globalThis) as Fetch,
  debugDirectory = join(sessionDir, "debug", "http"),
): Fetch | undefined {
  if (!settings.enabled) {
    return undefined;
  }

  const logger = new HttpDebugLogger(debugDirectory);
  return async (input, init) => {
    const requestId = await logger.reserveRequestId();
    const requestRecord: JsonRecord = {
      timestamp: new Date().toISOString(),
      direction: "request",
      method: requestMethod(input, init),
      url: requestUrl(input),
    };
    if (settings.include_request_headers) {
      requestRecord.headers = requestHeaders(input, init);
    }
    if (settings.include_request_body) {
      Object.assign(
        requestRecord,
        await captureRequestBody(input, init, settings.max_body_bytes),
      );
    }
    logger.log(`request${requestId}`, requestRecord);

    let response: Response;
    try {
      response = await nativeFetch(input, init);
    } catch (error) {
      logger.log(`response${requestId}`, {
        timestamp: new Date().toISOString(),
        direction: "response",
        url: requestUrl(input),
        error: error instanceof Error ? error.message : String(error),
      });
      throw error;
    }

    const responseRecord: JsonRecord = {
      timestamp: new Date().toISOString(),
      direction: "response",
      url: response.url || requestUrl(input),
      status: response.status,
      status_text: response.statusText,
    };
    if (settings.include_response_headers) {
      responseRecord.headers = responseHeaders(response);
    }
    if (settings.include_response_body) {
      void captureResponseBody(response, settings.max_body_bytes)
        .then((captured) => logger.log(`response${requestId}`, { ...responseRecord, ...captured }))
        .catch((error: unknown) =>
          logger.log(`response${requestId}`, {
            ...responseRecord,
            body_error: error instanceof Error ? error.message : String(error),
          }),
        );
    } else {
      logger.log(`response${requestId}`, responseRecord);
    }
    return response;
  };
}
