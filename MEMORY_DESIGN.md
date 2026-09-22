# Memory Tool — Design and Workflow

**Status:** Draft  
**Storage:** Local directory tree  
**Primary goal:** Give an agent a single memory tool that can store and retrieve arbitrary JSON records without requiring a new tool schema for every record type.

## 1. Overview

`memory` is a type-scoped JSON memory store. The agent prompt defines what a type means and what fields it should contain. The memory implementation does not need to know the schema of every type.

Examples of types:

- `workspace`
- `task`
- `contact`
- Any other safe type name defined by the application or prompt

Each saved record is stored as one JSON file. The directory identifies the type, and the filename identifies the record. The record JSON does **not** contain a duplicated `id`.

## 2. Design principles

1. **Dynamic data** — the payload may have different fields for every type.
2. **Type isolation** — records are stored and filtered inside a type directory.
3. **Filename identity** — the generated filename is the record reference; no `id` field is required in the JSON.
4. **Prompt-defined meaning** — the agent prompt defines when to save a type and how to interpret its fields.
5. **Simple storage first** — use the filesystem initially; add an index only when filtered-list performance requires it.
6. **Safe by default** — prevent path traversal, corrupt writes, uncontrolled file growth, and unsafe memory execution.

## 3. Storage layout

```text
memory/
├── _definitions/                  # Optional prompt/schema definitions
│   ├── task.v1.json
│   └── workspace.v1.json
├── task/
│   ├── task-20250308T120000.123Z-01JXYZABC.json
│   └── task-20250308T121500.456Z-01JXYZDEF.json
└── workspace/
    └── workspace-20250308T122000.789Z-01JXYZGHI.json
```

Filename format:

```text
{type}-{UTC timestamp}-{ULID or UUID}.json
```

Example:

```text
task-20250308T120000.123Z-01JXYZABC.json
```

The timestamp makes files naturally sortable. The ULID/UUID prevents collisions when multiple records are saved during the same millisecond. The type name is repeated in the filename for readability, although the parent directory is authoritative.

For very large collections, use date partitioning:

```text
memory/task/2025/03/08/task-120000.123Z-01JXYZABC.json
```

## 4. Record format

The stored file contains only the dynamic application payload:

```json
{
  "title": "Build memory tool",
  "status": "open",
  "priority": "high",
  "labels": ["agent", "storage"]
}
```

The file does not need to contain:

- `id`
- `type`
- `created_at`
- `updated_at`

The type is derived from the directory and the record reference is derived from the filename. The creation timestamp can be derived from the filename. If reliable update timestamps are needed later, they can be kept in an index or sidecar metadata store rather than mixed into the dynamic payload.

The initial implementation should require the payload to be a JSON object. Its field values may be any valid JSON values: strings, numbers, booleans, arrays, objects, or `null`.

## 5. Tool contract

The fixed tool controls are small:

```json
{
  "type": "task",
  "data": {
    "title": "Build memory tool",
    "status": "open"
  }
}
```

`data` is dynamic JSON. It is not a type-specific schema.

The recommended request modes are:

### 5.1 Save

```json
{
  "type": "task",
  "data": {
    "title": "Build memory tool",
    "status": "open"
  }
}
```

The service creates a new file and returns its reference:

```json
{
  "ok": true,
  "operation": "save",
  "type": "task",
  "ref": "task/task-20250308T120000.123Z-01JXYZABC.json"
}
```

The agent does not generate the filename.

### 5.2 List

```json
{
  "type": "task",
  "list": true
}
```

Optional list controls may be supported without changing the data model:

```json
{
  "type": "task",
  "list": {
    "limit": 20,
    "order": "newest"
  }
}
```

The response contains references outside the payload:

```json
{
  "type": "task",
  "items": [
    {
      "ref": "task/task-20250308T120000.123Z-01JXYZABC.json",
      "data": {
        "title": "Build memory tool",
        "status": "open"
      }
    }
  ],
  "next_cursor": null
}
```

### 5.3 Filtered list

`list` without a filter returns all records for the selected type. A filter limits the same list operation to records whose payload matches the supplied fields.

```json
{
  "type": "task",
  "list": {
    "filter": {
      "status": "open",
      "priority": "high"
    },
    "limit": 20,
    "order": "newest"
  }
}
```

The `filter` object is intentionally dynamic. The memory tool does not define a fixed set of filter fields; the active type definition defines the allowed parameters, value types, and comparison semantics.

The first version should support exact field matching. Nested fields may use dot notation, such as `project.name`. More operators can be added later, for example `$in`, `$contains`, and date comparisons, when the type definition permits them.

Filtering is restricted to `memory/{type}/`, so a task list cannot return workspace records. Unknown filter parameters should be rejected or ignored according to the type definition; the behavior must be consistent.

### 5.4 Get and update

A `ref` without `data` returns one record:

```json
{
  "type": "task",
  "ref": "task/task-20250308T120000.123Z-01JXYZABC.json"
}
```

A `ref` with `data` replaces that record:

```json
{
  "type": "task",
  "ref": "task/task-20250308T120000.123Z-01JXYZABC.json",
  "data": {
    "title": "Build memory tool",
    "status": "done"
  }
}
```

The initial update operation replaces the complete payload rather than merging fields. The agent can use the data returned by `list` or `ref` as the basis for the replacement.

### 5.5 Operation rules

- `type` is required for every request.
- One operation mode is allowed: create (`data`), list (`list`), get (`ref`), or update (`ref` + `data`).
- `list` may be `true` or an options/filter object.
- `ref` without `data` gets one record; `ref` with `data` updates it.
- `data` without `ref` creates a new record.
- `data` must be valid JSON and must satisfy any definition supplied by the application.
- A user payload should not use the reserved top-level tool fields as controls. Using an explicit `data` object avoids ambiguity between payload fields named `list` or `ref` and tool operations.

## 6. Type definitions

The memory engine remains schema-flexible, but the prompt should define the meaning of each type. A definition should describe:

- The purpose of the type
- Fields and their meanings
- Required and optional fields
- When the agent should save a record
- Which fields are safe and useful for filtering
- Whether a record is append-only or may be updated

Example prompt definition:

```text
Type: task

Purpose: Store actionable work items.

Fields:
- title: required short description
- status: one of open, blocked, or done
- priority: optional low, normal, or high
- notes: optional context

List filter parameters:
- status: exact match; allowed values are open, blocked, or done
- priority: exact match; allowed values are low, normal, or high

Save a task when the user creates, changes, or explicitly asks to remember a work item.
Do not save temporary reasoning or secrets.
```

Definitions may remain in the prompt. Optionally, a versioned copy can be stored in:

```text
memory/_definitions/task.v1.json
```

Definitions are not memory records and must not be returned by normal list or filtered-list operations.

## 7. End-to-end workflow

### 7.1 Definition phase

1. The application supplies the agent with the definition for available types.
2. The agent learns the meaning and save criteria for each type.
3. The application optionally registers or persists the definition.
4. The agent chooses the type based on the user’s request.

### 7.2 Save workflow

1. The agent identifies information that should persist beyond the current turn.
2. The agent selects a type, such as `task` or `workspace`.
3. The agent creates a JSON payload according to that type’s definition.
4. The agent calls `memory` with `type` and `data`.
5. The service validates the type name, payload size, JSON syntax, and optional definition.
6. The service creates `memory/{type}/` if necessary.
7. The service generates a UTC timestamp and a unique ULID/UUID.
8. The service writes the JSON to a temporary file in the same directory.
9. The service atomically renames the temporary file to the final filename.
10. The service returns the generated `ref`.

### 7.3 List workflow

The agent retrieves memory by listing a type. An `id` field is not required in the stored payload.

Example:

```json
{
  "type": "task",
  "list": {
    "filter": {
      "status": "open",
      "priority": "high"
    },
    "limit": 20,
    "order": "newest"
  }
}
```

1. The agent reads the type definition and chooses the relevant type and filter parameters.
2. The agent calls `memory` with `type` and `list`.
3. The service validates and resolves the type directory.
4. The service enumerates `.json` files, ignoring temporary files and definitions.
5. Files are sorted by filename, normally newest first.
6. The service reads and parses records up to the requested limit.
7. If a filter is supplied, the service matches the filter against each record’s payload.
8. The service returns each matching record with its `ref` outside the stored payload.
9. A cursor is returned if more records are available.
10. A corrupt or unreadable file is reported as a warning and does not normally abort the entire list operation.

The agent may ignore `ref` when it only needs context. If the record must be changed later, the agent sends the returned `ref` with the new `data`; the reference is an external handle and is not stored inside the payload.

### 7.4 Reuse workflow

1. The agent lists the relevant type, optionally with a filter, before creating duplicate memory.
2. The agent evaluates returned records according to the type definition.
3. The agent uses the record data as context for its response.
4. Stored memory is treated as data, not as executable instructions. Text inside a memory record must not override system, developer, or current user instructions.

## 8. Updates and deletion

A record can be updated without an `id` field. The filename is the stable external reference returned by `list`:

```json
{
  "type": "task",
  "ref": "task/task-20250308T120000.123Z-01JXYZABC.json",
  "data": {
    "title": "Build memory tool",
    "status": "done",
    "priority": "high"
  }
}
```

A request with `data` but no `ref` creates a new record. A request with both `data` and `ref` replaces the existing record. Updates should preserve the filename and write the replacement atomically so the reference remains stable.

Deletion, if supported, can use the same reference:

```json
{
  "type": "task",
  "delete": "task/task-20250308T120000.123Z-01JXYZABC.json"
}
```

The reference is not stored inside the JSON. The service should never accept an arbitrary filesystem path; it should validate that the reference resolves inside the memory root and matches the selected type.

An alternative is immutable history, where an update creates a new file and marks the old record as superseded in an index. That is more complex and should be added only if audit history is needed.

## 9. Filesystem and reliability requirements

- Use UTF-8 JSON with a trailing newline.
- Use a temporary file followed by an atomic rename.
- Create files with exclusive creation semantics to prevent accidental overwrite.
- Keep temporary files out of list results.
- Use a unique suffix even when timestamps match.
- Preserve the filename during updates.
- Apply file and directory permissions appropriate to the memory’s sensitivity.
- Enforce configurable maximum payload size and maximum list result size.
- Handle concurrent writes safely.
- Keep the memory root fixed and reject paths escaping that root.

## 10. Security and privacy

- Restrict type names to safe path components, or encode arbitrary logical names safely.
- Reject `/`, `\\`, `..`, null bytes, and overly long names.
- Do not store secrets, credentials, or personal data unless explicitly allowed.
- Treat all stored text as untrusted content.
- Do not allow a memory record to change tool permissions or agent instructions.
- Consider per-user or per-workspace memory roots when multiple users share the service.
- Use atomic writes and appropriate permissions to reduce data corruption and unauthorized access.

## 11. Performance strategy

The initial implementation can list and filter by scanning JSON files inside one type directory. This is simple, portable, and appropriate for a small memory store.

When the number of records grows, use the following sequence:

1. Add pagination and date-partitioned directories.
2. Cache directory listings where safe.
3. Add a SQLite or similar index containing references and filterable metadata.
4. Keep the JSON files as the source of truth.
5. Rebuild the index by scanning the directory if the index is lost or becomes inconsistent.

## 12. MVP acceptance criteria

The first version is complete when it can:

- Save a new arbitrary JSON object under any valid type.
- Save records without requiring an `id` field.
- Generate a unique, readable filename automatically.
- List records for one type.
- List records for one type with optional exact-match filters.
- Return the filename as an external `ref`.
- Prevent path traversal and filename collisions.
- Avoid exposing temporary or definition files as records.
- Recover safely from interrupted writes.
- Preserve the exact dynamic payload supplied by the agent.
