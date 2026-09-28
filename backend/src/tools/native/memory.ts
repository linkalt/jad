import type {NativeMemoryToolSettings} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import {executionError, renderToolResult} from "../output.js";
import type {ToolDefinition, ToolExecutionContext} from "../types.js";
import {MemoryConflictError, MemoryStore, MemoryStoreError, type MemoryItem} from "./memory-store.js";

export const NATIVE_MEMORY_ID = "native.memory";
export const nativeMemoryDefinition: ToolDefinition = {
  id: NATIVE_MEMORY_ID, source: "native", name: "memory",
  description: "Store and retrieve bounded JSON-object records. `create` assigns and returns the record's ref; never invent one or create a duplicate just to obtain a ref. Reuse returned refs with `get`/`update`, and list first when checking for an existing record.",
  inputSchema: {type: "object", required: ["operation", "type"], additionalProperties: false, properties: {
    operation: {type: "string", enum: ["create", "get", "list", "update"], description: "Create assigns and returns a ref for a new record. Use that exact returned ref for get/update; never invent a ref or create a duplicate just to obtain one. List first if checking for an existing record."},
    type: {type: "string", minLength: 1}, ref: {type: "string", minLength: 1, description: "For get/update, use the exact ref returned by create or list; never invent one. Create assigns and returns its ref."},
    data: {type: "object"}, filter: {type: "object", description: "Top-level or dotted-path exact-match filter; omitted values inherit from a persistent cursor."}, limit: {type: "integer", minimum: 1},
    order: {type: "string", enum: ["newest", "oldest"], description: "List order; when omitted while continuing, the cursor order is used."}, cursor: {type: "string", minLength: 1, description: "Cursor returned by list; a cursor position must still exist."}, if_match: {type: "string", minLength: 1, description: "Optional current ETag for optimistic concurrency. On mismatch, the update is rejected and the current_etag is returned; omitted means an unconditional update."},
  }, allOf: [
    {if: {properties: {operation: {const: "create"}}}, then: {properties: {data: {}}, required: ["data"]}},
    {if: {properties: {operation: {const: "get"}}}, then: {properties: {ref: {}}, required: ["ref"]}},
    {if: {properties: {operation: {const: "update"}}}, then: {properties: {ref: {}, data: {}}, required: ["ref", "data"]}}, 
  ]},
};
type Operation = "create" | "get" | "list" | "update";
interface MemoryContext extends ToolExecutionContext { policy: NativeMemoryToolSettings; store: MemoryStore; }

export async function collectMemoryStateContext(
  store: MemoryStore,
  policy: NativeMemoryToolSettings,
  selectors: NativeMemoryToolSettings["statecontext"],
): Promise<JsonRecord> {
  const items: JsonRecord[] = [];
  const queries: JsonRecord[] = [];
  for (const selector of selectors) {
    const exclude = selector.exclude ?? {};
    const excludeAny = selector.exclude_any ?? [];
    const hasExclude = Object.keys(exclude).length > 0;
    const hasExcludeAny = excludeAny.length > 0;
    const listed = await store.list(selector.type, selector.filter, selector.limit, "newest", undefined, policy, {
      createCursor: false,
      ...(hasExclude ? {exclude} : {}),
      ...(hasExcludeAny ? {excludeAny} : {}),
    });
    const exclusion = {
      ...(hasExclude ? {exclude} : {}),
      ...(hasExcludeAny ? {exclude_any: excludeAny} : {}),
    };
    items.push(...listed.items.map(({ref, data}) => ({type: selector.type, ref, filter: selector.filter, ...exclusion, data})));
    queries.push({type: selector.type, filter: selector.filter, ...exclusion, returned: listed.items.length, has_more: listed.hasMore});
  }
  return {items, queries, has_more: queries.some((query) => query.has_more === true)};
}
export function normalizeMemoryArguments(args: JsonRecord): JsonRecord {
  const normalized = {...args};
  if (typeof normalized.limit === "number" && Number.isSafeInteger(normalized.limit)) {
    normalized.limit = Math.max(1, normalized.limit);
  }
  return normalized;
}
function fail(id: string, code: string, message: string): string { return executionError(id, "invalid_input", code, message); }
function itemInformation(item: MemoryItem, operation: Operation): Array<[string, unknown]> { return [["operation", operation], ["type", item.ref.split("/")[0]], ["ref", item.ref], ["etag", item.etag]]; }
function renderItem(id: string, operation: Operation, item: MemoryItem, summary: string): string {
  return renderToolResult({
    id,
    status: "ok",
    summary,
    information: itemInformation(item, operation),
    ...(operation === "get" ? {
      content: {encoding: "utf8" as const, text: JSON.stringify(item.data, null, 2)},
    } : {}),
  });
}
function object(value: unknown): value is JsonRecord { return typeof value === "object" && value !== null && !Array.isArray(value); }
function parseOperation(value: unknown): Operation | undefined { return value === "create" || value === "get" || value === "list" || value === "update" ? value : undefined; }

export async function executeNativeMemory(args: JsonRecord, context: MemoryContext): Promise<string> {
  const action = parseOperation(args.operation); const type = typeof args.type === "string" ? args.type : undefined;
  if (!action || !type) return fail(context.callId, "invalid_operation", "Memory requires a valid operation and type");
  try {
    if (action === "create") {
      if (!object(args.data) || Object.keys(args).some((key) => !["operation", "type", "data"].includes(key))) return fail(context.callId, "invalid_create", "Create requires only type and an object data payload");
      return renderItem(context.callId, action, await context.store.create(type, args.data, context.policy), "Memory record created");
    }
    if (action === "get") {
      if (typeof args.ref !== "string" || Object.keys(args).some((key) => !["operation", "type", "ref"].includes(key))) return fail(context.callId, "invalid_get", "Get requires only type and ref");
      return renderItem(context.callId, action, await context.store.get(type, args.ref, context.policy), "Memory record retrieved");
    }
    if (action === "update") {
      if (typeof args.ref !== "string" || (args.if_match !== undefined && typeof args.if_match !== "string") || !object(args.data) || Object.keys(args).some((key) => !["operation", "type", "ref", "if_match", "data"].includes(key))) return fail(context.callId, "invalid_update", "Update requires type, ref, and an object data payload; if_match is optional");
      return renderItem(context.callId, action, await context.store.update(type, args.ref, typeof args.if_match === "string" ? args.if_match : undefined, args.data, context.policy), "Memory record updated");
    }
    if (!object(args.filter ?? {}) || Object.keys(args).some((key) => !["operation", "type", "filter", "limit", "order", "cursor"].includes(key))) return fail(context.callId, "invalid_list", "List accepts type, filter, limit, order, and cursor");
    const requested = typeof args.limit === "number" && Number.isSafeInteger(args.limit) ? args.limit : context.policy.list_limit;
    const listed = await context.store.list(type, args.filter === undefined ? undefined : args.filter as JsonRecord, Math.min(Math.max(1, requested), context.policy.max_list_limit), args.order, args.cursor, context.policy);
    const text = JSON.stringify({items: listed.items, ...(listed.nextCursor ? {next_cursor: listed.nextCursor} : {}), ...(listed.warnings.length ? {warnings: listed.warnings} : {})}, null, 2);
    return renderToolResult({id: context.callId, status: "ok", summary: `${listed.items.length} memory record${listed.items.length === 1 ? "" : "s"} listed`, information: [["operation", "list"], ["type", type], ["count", listed.items.length], ...(listed.nextCursor ? [["has_more", true] as [string, unknown]] : [])], content: {encoding: "utf8", text}});
  } catch (error) {
    if (error instanceof MemoryConflictError) return executionError(context.callId, "error", "memory_conflict", error.message, [
      ["operation", "update"], ["type", type], ["ref", args.ref], ["current_etag", error.currentEtag],
    ]);
    if (error instanceof MemoryStoreError) return executionError(context.callId, error.message.includes("not found") ? "not_found" : "error", "memory_storage_error", error.message);
    return executionError(context.callId, "error", "memory_operation_failed", error instanceof Error ? error.message : String(error));
  }
}
