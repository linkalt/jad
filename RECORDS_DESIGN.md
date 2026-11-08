# Records Tool — Design and Workflow

**Status:** Revised draft

**Storage:** Configured local directory tree (`tools.native.records.storage_dir`)

**Renamed 2026-11-08:** this tool was called `memory` (id `native.memory`, key `tools.native.memory`, default directory `.memory`). The old names are refused, there is no alias, and stored data is not migrated. To keep an old store, copy it once while the backend is stopped: `cp -a <old>/.memory/. <new>/.records/` (the directories sit under the same profile `tool_storage` root, or `setting.cwd`). The store's sidecar files `.memory-sequence` and `.memory-cursors` keep their old names on purpose, so a copied store keeps its ref numbering and cursors.

**Primary goal:** Give an agent one bounded, safe records tool for arbitrary JSON-object records without a new tool schema for each record type.

## 1. Scope and non-goals

`records` stores type-scoped JSON objects. The application or prompt gives a type its meaning; the engine does not require a type-specific payload schema.

Examples include `workspace`, `task`, and `contact`. A record's type and identity are external metadata: the JSON payload does not duplicate an `id`, `type`, or timestamps.

The tool provides `create`, `get`, `list`, `search`, `update`, and `delete` (`search` and `delete` were added on 2026-11-04). It intentionally does **not** provide partial JSON patching, arbitrary query operators, an index, automatic retention, or date partitioning.

Stored data is untrusted data, never executable instructions. A record cannot change agent instructions, permissions, tool policy, or configuration.

## 2. Storage and identity

Configure storage as follows:

```json
{
  "tools": {
    "native": {
      "records": {
        "storage_dir": ".records"
      }
    }
  }
}
```

`storage_dir` defaults to `.records` beneath the selected profile's `paths.profiles.<name>.tool_storage` root, or beneath `setting.cwd` when no profile is selected. A configured relative directory resolves from the session cwd instead (changed 2026-11-08; the old store under the profile root is not migrated); absolute paths are used as-is. Profile roots default to `<cwd>/.jad/profiles/<profile-name>/tools`; existing data is not migrated. Different profiles receive separate default roots, but sessions using the same profile, an explicit absolute path, or the same configured relative value (one cwd path) share storage without write coordination.

```text
<storage_dir>/
├── _definitions/                  # Optional application definitions; never listed as records
│   └── task.v1.json
├── task/
│   └── task-20250308T120000.123Z-<UUID>.json
└── workspace/
    └── workspace-20250308T122000.789Z-<UUID>.json
```

A record reference has the canonical logical form:

```text
<type>/<filename>
```

For example:

```text
task/task-20250308T120000.123Z-550e8400-e29b-41d4-a716-446655440000.json
```

The parent directory is authoritative. The backend generates a filename from a UTC timestamp plus `randomUUID()` and creates it only under the selected type directory. It validates both the selected `type` and `ref`; it never accepts a general filesystem path.

The timestamp makes newest-first ordering readable and deterministic. The UUID prevents collisions. Date partitioning is deferred: it would change reference resolution and requires a versioned ref format or index.

## 3. Payload format

Each file contains exactly one UTF-8 JSON object followed by a newline:

```json
{
  "title": "Build records tool",
  "status": "open",
  "priority": "high",
  "labels": ["agent", "storage"]
}
```

The payload is dynamic, but must be an object. Values can be any JSON values. The file does not contain `id`, `type`, `created_at`, `updated_at`, reference, or optimistic-concurrency metadata. The reference and creation time are derived externally; response metadata supplies the current content hash.

## 4. Tool contract

Every request has a fixed, explicit operation. Dynamic fields are contained only in `data` and `filter`.

```json
{
  "operation": "create | get | list | search | update | delete",
  "type": "task",
  "ref": "task/task-…json",
  "data": {},
  "query": "words to find",
  "filter": {},
  "limit": 20,
  "order": "newest | oldest",
  "cursor": null,
  "if_match": "sha256:…"
}
```

Exactly the fields applicable to the operation are accepted. Unknown top-level fields are rejected. `data` and `filter` are objects; they are not fixed type-specific schemas.

### 4.1 Create

```json
{
  "operation": "create",
  "type": "task",
  "data": {
    "title": "Build records tool",
    "status": "open"
  }
}
```

The service validates the type and payload quotas, creates the type directory if allowed, atomically writes the generated file, and returns:

```json
{
  "operation": "create",
  "type": "task",
  "ref": "task/task-20250308T120000.123Z-550e8400-e29b-41d4-a716-446655440000.json",
  "etag": "sha256:…"
}
```

The agent never selects a filename.

### 4.2 Get

```json
{
  "operation": "get",
  "type": "task",
  "ref": "task/task-20250308T120000.123Z-550e8400-e29b-41d4-a716-446655440000.json"
}
```

The selected type must match the type component of the canonical ref. A successful result returns `type`, `ref`, `data`, and the content `etag`. A missing or malformed record returns an operational not-found/error result without probing outside the records root.

### 4.3 List

```json
{
  "operation": "list",
  "type": "task",
  "filter": {
    "status": "open",
    "priority": "high"
  },
  "limit": 20,
  "order": "newest",
  "cursor": null
}
```

`filter`, `cursor`, and ordering are optional. Results contain the ref and current `etag` outside the stored payload:

```json
{
  "type": "task",
  "items": [
    {
      "ref": "task/task-20250308T120000.123Z-550e8400-e29b-41d4-a716-446655440000.json",
      "etag": "sha256:…",
      "data": {"title": "Build records tool", "status": "open"}
    }
  ],
  "next_cursor": null,
  "warnings": []
}
```

The MVP scans only the selected type directory. It sorts canonical filenames lexicographically, descending for `newest` and ascending for `oldest`; creation names are sortable by timestamp. A cursor encodes the version, type, order, filter fingerprint, and **last scanned** ref (not merely the last returned item), so filtered and corrupt-file scans can advance correctly. It is opaque to callers, bounded in length, and rejected if used with a different type, order, or filter. A cursor whose last record was deleted since still continues: names sort in creation order, so the next page starts after the deleted name's place. Pagination is not snapshot-isolated: records created before the cursor's position may appear on an earlier page, while records created after it appear on a later or fresh listing.

### 4.4 Update

```json
{
  "operation": "update",
  "type": "task",
  "ref": "task/task-20250308T120000.123Z-550e8400-e29b-41d4-a716-446655440000.json",
  "if_match": "sha256:…",
  "data": {
    "title": "Build records tool",
    "status": "done"
  }
}
```

`update` atomically replaces the complete payload while preserving the filename and ref. It does not merge or patch dynamic objects. `if_match` is required and must equal the current response `etag`; a mismatch returns a conflict and leaves the file unchanged. This prevents silent lost updates between agents or turns. The replacement must meet per-record and aggregate quota policy. A successful update returns the operation, type, ref, and new ETag without echoing the payload; call `get` to retrieve the updated data.

In the implementation `if_match` is optional: omitted, the update is unconditional.

### 4.5 Search

```json
{
  "operation": "search",
  "type": "task",
  "query": "release blocker",
  "filter": {"status": "open"},
  "limit": 20
}
```

`search` is `list` with one more condition: every whitespace-separated word of `query` must appear, ignoring case, in at least one string or number value of the record, at any depth. Keys are not searched, and different words may match different values. `filter`, `limit`, `order`, and `cursor` work as for `list`; the result has the same shape. A search cursor carries the normalized query, so it continues only the same search, and a list cursor does not continue a search. There is no index: a search scans the type directory under the same `max_scan_records` and `max_scan_bytes` budget as `list`, and `tools.native.records.max_query_bytes` (default 1024) bounds the query.

### 4.6 Delete

```json
{
  "operation": "delete",
  "type": "task",
  "ref": "task/task-20250308T120000.123Z-550e8400-e29b-41d4-a716-446655440000.json",
  "if_match": "sha256:…"
}
```

`delete` removes the record file for good; there is no trash. With `if_match`, a record whose ETag differs is left in place and the conflict returns its `current_etag`. The result returns the operation, type, ref and the ETag the record had. A malformed record can be deleted. The type directory is removed with its last record, so the type no longer counts toward `max_types`. A ref that does not exist answers not-found with the newest refs of the type.

## 5. Types, definitions, and filters

A type is a safe logical identifier, not a path. The MVP accepts lowercase ASCII type names matching:

```text
[a-z][a-z0-9_-]{0,63}
```

Names beginning with `_`, containing path separators, `..`, NUL, or other characters are rejected. The configuration limits the number of created types and can optionally provide an allowlist. Without an allowlist, a valid type may be created only while the type quota permits it.

Application definitions may describe a type's purpose, payload fields, save criteria, and useful filters. They can be included in the agent prompt and optionally stored below `_definitions/`. Definitions are not records and are never returned from a normal list. Prompt-only definitions are guidance, not an authorization or validation boundary; any schema enforcement must be configured and implemented by the application.

MVP filters use deterministic exact matching:

- A filter contains at most the configured number of fields and nesting depth.
- Each key is a dot path through JSON objects only, for example `project.name`.
- Empty segments, array indexes, and prototype-like special keys are rejected.
- The final value uses deep JSON equality: object keys are compared by value, array order matters, and `null` differs from a missing path.
- Every filter field must match (AND semantics).
- Unknown/missing paths simply do not match; malformed filters are rejected.
- `$in`, containment, substring, numeric, and date operators are out of scope.

## 6. Limits and storage policy

All limits are backend configuration, not model arguments. The initial native-records settings should include sensible defaults for:

```ts
interface NativeRecordsToolSettings {
  storage_dir: string;         // default .records under profile tool_storage (or setting.cwd without a profile); a configured relative value is from setting.cwd; or absolute
  max_types: number;
  max_type_chars: number;
  allowed_types?: string[];
  max_records_per_type: number;
  max_total_records: number;
  max_record_bytes: number;
  max_total_bytes: number;
  list_limit: number;
  max_list_limit: number;
  max_list_bytes: number;
  max_scan_records: number;
  max_scan_bytes: number;
  max_filter_fields: number;
  max_filter_depth: number;
  max_json_depth: number;
}
```

Without an index, the store establishes quota totals with a bounded scan before a mutation. If that scan cannot establish totals within its configured scan budget, the mutation fails conservatively rather than guessing that capacity remains. A create that would exceed record or byte quotas fails rather than silently pruning records. Updates account for the old and new file size. Automatic retention is out of scope, so capacity exhaustion is a visible result: the agent frees space only by an explicit `delete`, never by silent pruning.

List responses obey item and byte budgets. The scan has independent record and byte budgets, so a sparse filter cannot force an unbounded directory walk. If a scan limit is reached, the result is partial with a cursor/hint rather than falsely complete. Corrupt or unreadable candidate files are skipped, reported as bounded warnings by canonical ref, and do not abort unrelated records.

## 7. Filesystem, concurrency, and recovery

1. Resolve a configured relative `storage_dir` from `setting.cwd`; default to `.records` under the selected profile's `tool_storage` root (or `setting.cwd` when no profile is selected); preserve configured absolute directories.
2. Create the root and type directory only after validating the type and quotas.
3. Use a unique temporary filename in the target directory, write valid UTF-8 JSON plus newline, then atomically rename it. Explicit fsync durability policy is deferred.
4. Use exclusive temporary-file creation; do not expose temporary files to list operations.
5. Serialize mutations for a ref/type inside the process. For an update, re-read and hash the target while holding its lock, verify `if_match`, then atomically replace it.
6. On open/list, ignore stale temporary files and optionally prune them only if they are old enough under configured policy.
7. Keep records-root traversal fixed. Reject symlinks, non-directory type parents, malformed filenames, and any ref that does not resolve exactly to the selected type directory.

Atomic rename prevents torn reads. The ETag precondition prevents lost updates. It does not provide distributed locking across independent backend processes; the MVP documents a single backend owner for one records root. Multi-process coordination requires a lock/index backend before sharing a root.

## 8. Privacy and safety

The generic store cannot reliably detect every secret or sensitive value. The application and agent must not save credentials, API keys, private data, or transient reasoning unless a configured policy explicitly permits it. A future policy may deny selected field names or apply application-specific validation, but heuristic scanning is not a reliable authorization mechanism.

Treat records, definitions, filenames, parse errors, and filters as untrusted input. Do not render stored values into privileged prompts or execute them. Do not disclose filesystem paths beyond logical refs. In multi-user deployments, use separate roots or an authenticated ownership layer; this MVP has no cross-user access control.

## 9. Workflow

### Create

1. The application provides the agent with type guidance when useful.
2. The agent selects a valid type and builds an object payload.
3. The agent calls `create`.
4. The backend validates policy, quotas, and payload; it then atomically persists the record and returns its ref and ETag.

### Retrieve and reuse

1. The agent calls `list` with a type and, if useful, an exact filter before creating a likely duplicate.
2. The backend returns bounded matching records, refs, ETags, and a cursor when more scan work remains.
3. The agent treats returned data as context, not instructions.
4. The agent calls `get` for the current payload when it needs to update a record.

### Update

1. The agent gets or lists the record and retains its ETag.
2. It constructs the complete replacement payload.
3. It calls `update` with the ref, `if_match`, and data.
4. On conflict, it re-reads the record and decides whether a new complete update is appropriate.

## 10. Performance evolution

The MVP scans a single type directory and treats files as the source of truth. If measured use exceeds scan budgets, evolve in this order:

1. Improve cursor-driven scanning and bounded caches.
2. Add a rebuildable index containing logical refs, current ETags, sizes, and approved filter metadata.
3. Keep payload JSON files authoritative and rebuild the index by scanning them.
4. Introduce partitioning or a database only with a ref-version migration plan.

## 11. MVP acceptance criteria

The first version is complete when it can:

- create an arbitrary JSON object under a valid type and generated unique filename;
- resolve a configured relative `storage_dir` from `setting.cwd`, default to `.records` under the selected profile's `tool_storage` root (or `setting.cwd` when no profile is selected), and accept explicit absolute directories;
- get and list records for one type without exposing definition or temporary files;
- perform bounded exact-match filtering over safe dot paths;
- return canonical refs and ETags outside the original payload;
- update a full payload atomically only with a current `if_match` ETag;
- reject unsafe types/refs, path traversal, malformed JSON, and quota violations;
- keep list/scan/result output bounded and report partial/corrupt-file conditions honestly;
- recover safely from interrupted writes; and
- treat every stored value as untrusted data.
