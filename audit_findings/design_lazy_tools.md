# Design: lazy tool loading (load_tool)

Scope: `load_tool`, a native tool that lists a profile's lazy tools and shows, or loads, one of
them by name. A lazy tool is absent from the tools array sent to the model until `load_tool` has
loaded it; after a load it is present with its real schema, from the next request. Config:
`paths.profiles.<name>.tools.lazy` and `tools.lazy_summary_bytes`.

## Config

- `tools.lazy` takes the `tools.allow` entry forms (native name, `mcp:<server>:<tool>`,
  `mcp:<server>:*`). `load_tool` itself cannot be lazy.
- `load_tool` is a normal native tool, enabled only by a `tools.native.load_tool` key (`{}`, with no
  keys of its own). It is in `tools.native`'s supported keys and in `setting.schema.json`, and it
  needs `load_tool` in `tool_definition_paths` like every native tool. The app never adds it.
  If the key is absent the tool is absent.
- Dependency: a profile whose `tools.lazy` is non-empty while `tools.native.load_tool` is not
  enabled is refused at load (`checkLazyNeedsLoadTool`). The error names both keys. Nothing else
  can show a lazy tool, so the profile is not run silently without them.
- With `tools.native.load_tool` enabled and no `tools.lazy`, the tool is listed in the tools array
  and lists "No lazy tools.". It is not refused: a toolset is shared across profiles, and a profile that does
  not use lazy tools should not be broken by the toolset it names.
- A lazy entry must name a tool the profile shows and the lazy pass can bind: a denied tool, one outside a
  non-empty `allow`, or one of `subagent`, `profile_message`, `transcript`, `transcripts`, `profile` and
  `load_tool` is a configuration error (`checkLazyEntriesVisible`, `native.NotLazyBindable`).
- A profile with `tools.lazy` whose `allow` omits `load_tool`, or whose `deny` names it, is refused at load
  (`checkLazyLoadToolShown`): the lazy tools could never load, the same silent failure as no key at all.
- `tools.lazy_summary_bytes` is a sibling of `lazy` in the profile's tools object, default 256,
  range 1 to 4096. Summaries are cut on a rune boundary.
- `tools.lazy_load_max` is the most names one `load_tool` call may load: default 16, range 1 to 100.
- Lazy needs a catalog layout. A session without a state folder is a configuration error.

## Fingerprint and state file

- Fingerprint = SHA-256 over the sorted, de-duplicated `tools.lazy` entry strings and the summary
  bound. The profile name, session ID and registered tool list are not part of it.
  A wildcard that gains a tool is therefore refused until loaded, and a config change clears the
  loaded set.
- File: `<profile state folder>/lazy_tools.json`, `{"fingerprint": ..., "loaded": [sorted IDs]}`.
  The profile state folder is `<state>/<session>/<category>/<name>`, the folder that holds the
  profile's `messages.jsonl`. It is per profile, not per session: two profiles of one session
  with different `tools.lazy` would otherwise clear each other's loaded set on every open.
- The set is opened from the file once per prepare; there is no cache. A missing or corrupt file, or
  a fingerprint mismatch, gives an empty set and rewrites the file. Missing or unreadable is logged.
- A load writes the file through a temporary file and a rename, after it has checked the name.
  A name that is not a bound lazy tool is refused and nothing is written.
- A loaded ID whose tool no longer exists stays in the file and is not callable.

## Gate placement

- Native lazy tools: `native.BuildCatalogWithAsync` takes the set as its last argument (nil means no
  gating). The native pass binds its tools, gates the lazy ones, then wraps every native tool with
  its event hooks once. Subagent child catalogs are built from that gated list, so a child gets the
  owner's gate on its native lazy tools too: a child's call to an unloaded one is refused with
  `tool_not_loaded`. The gate is one set for the owner and its children, so a load by either clears it.
- MCP lazy tools: `buildRegisteredCatalog` binds the MCP tools to the same set, gates the lazy ones,
  then applies `WithToolEventHooksAll`. The raw MCP list goes to `SubagentMCP.BindToolsets`. For each
  child, a session MCP tool is gated with the same set (`GateFor`) and then hooked with the owner's
  hooks (`WithToolEventHooks`), so a child's lazy MCP tool is refused until loaded and its call fires
  the owner's pre_tool and post_tool once. A toolset's MCP tool is not hooked again there: the resolver
  already hooked it with its own profile's hooks.
- `Bind` skips a tool already bound and `Gate` skips a tool already gated, so no tool is wrapped
  twice.
- The stub design is reversed. A lazy tool is no longer sent to the model as a summary with a
  permissive `{"type":"object"}` schema. The stub schema, its validator and the stub wrapper are gone.
- Accepted trade-off: the tools array is part of the provider's prompt cache prefix, so each load
  batch rewrites that cache once, on the next request. Tools are added in place in catalog order and
  nothing is reordered, so only the request after a load pays this. The native and MCP entries are disjoint: an `mcp:` entry matches only MCP tools.
- Order at call time: the loaded check, then the real schema, then the tool. An unloaded lazy call is
  refused with `tool_not_loaded` whatever its arguments, and the real schema is never run against it.
  The refusal runs inside the tool's own wrappers, so its pre_tool and post_tool hooks fire once, like
  any tool call. A loaded call is checked against the real schema as an eager tool would be.
- The tools array is built per request: `Run` rebuilds the array in every round, and `ResponseTools`
  lists only the tools whose `Listed` says so. A lazy tool's `Listed` is its loaded state, and
  `load_tool` is listed whenever it is enabled, even with no lazy tool bound or every lazy tool loaded:
  the key alone is the switch. Dispatch uses the full catalog, so a name in history still reaches the
  gate.
- A gated tool runs its real tool through a catalog of its own, so validation against the real
  schema, the report bound and the output shape match an eager tool.

## load_tool

- Listed only when `tools.native.load_tool` is enabled. It is built with the other native tools, and
  it is hooked like every native tool.
- It is not in the child pool: it is added to the catalog after the subagent tool, so no child catalog
  gets it from the pool. A child gets it only when the owner enables it and the agent names it
  (`SetLazy`, `addLoadToolToNestingAgents`).
- It reads the set at call time, so it sees every bound tool, native and MCP.
- Input: `names`, an array of strings with minItems 1 and uniqueItems. There is no `name` alias: a
  call that still sends `name` is refused by the validator. Duplicates are refused by the validator
  too; they are not de-duplicated. The schema has no maxItems and no pattern grammar: each entry is
  a tool name or a `tools.lazy`-style pattern, and the description says it is limited to lazy tools
  and `tools.lazy_load_max`.
- Each entry is one of: a tool name (`read`, the provider name); `mcp:<server>:<tool>`; `mcp:<server>:*`
  (every lazy tool of that server); or `*` (every lazy tool). Patterns use the grammar of `tools.lazy`
  (`tools.EntryMatches`): `*` appears only as the trailing server part of `mcp:<server>:*`, never
  inside a native name or a tool part. `*` alone is the one addition, and it has no other form.
- Match scope: a pattern selects only bound lazy tools. A tool the profile hides, a non-lazy tool,
  and a tool no `tools.lazy` entry selects never match. A non-lazy name that a pattern could match
  gets the same line as an unknown name.
- No names: one `name — summary` line per lazy tool.
- Order of a call: schema validation (including uniqueItems), then the batch limit, then the
  per-name lookup against the catalog at call time.
- Batch limit: `tools.lazy_load_max` (default 16, range 1 to 100), a sibling of
  `tools.lazy_summary_bytes`. It counts matched tools, not pattern strings: the patterns are expanded,
  the distinct tools are counted, and a count over the limit refuses the whole call with a
  `batch_too_large` tool error ("matched N lazy tools, the limit is M (tools.lazy_load_max)"), and
  nothing is loaded. It is enforced at runtime, not in the schema.
- Per pattern: a pattern or name that selects one or more lazy tools loads them. A pattern that
  selects none gets the line "no lazy tool matches <pattern>"; a name that is not a lazy tool gets
  "not a lazy tool of this profile". The other patterns still load. A tool already loaded is shown
  again and does not change the file.
- Overlap: a tool selected by several patterns is loaded once and counted once. Its full definition
  appears once, under the first section that selects it; a later section that selects it says
  "already listed above".
- Output: one text block, one section per input pattern, in the order sent, separated by a rule
  line. Under each section the matched tools are listed in sorted tool-ID order.
- Writes: the loaded set is saved once per call, after the lookup, and only when a matched tool was
  not loaded before. The save uses a temporary file and a rename. A call where no pattern matches
  writes nothing.
- Writes: the loaded set is saved once per call, after the lookup, and only when a matched name was
  not loaded before. The save uses a temporary file and a rename. An all-unknown batch writes
  nothing.

## Behaviour checked by tests

- A refused call returns `tool_not_loaded` naming `load_tool`; a loaded call runs (native and MCP).
- A refused call hands its wrappers the arguments the model sent, so a pre_tool command sees them. The
  arguments are not checked against the real schema, and a call whose arguments are not a JSON object
  gives its wrappers an empty map.
- A batch loads several tools in one call and one write; an unknown name does not block the others;
  an all-unknown batch writes nothing; a batch over `tools.lazy_load_max` loads nothing.
- A pattern expands to the lazy tools it selects; the limit counts those tools; a pattern with no
  match gets its own line and the others load; overlapping patterns load and show a tool once; `*`
  is refused only by the limit; a hidden or non-lazy tool that a pattern matches is not loaded.
- The tools array is filtered per request: a lazy tool is absent before load and present, with its
  real schema, after load. Its order is catalog order, the same after a restart over the same state
  folder, and a load never reorders the rest.
- A load result begins `Loaded. Available from the next request.`
- A batch that matches nothing changes nothing: no write, no next-request sentence.
- A call by name to an unloaded lazy tool (for example a name in history after a restart) gets
  `tool_not_loaded`.
- A restart over the same state folder restores the loaded set.
- Each refused or loaded call fires its pre_tool hook exactly once, and a refused call also fires its
  post_tool hook. A refused call never runs the real tool.
- A child's own `profile_message` and nested `subagent` calls fire the owner's pre_tool hook once each,
  as the parent's calls do (`NewSubagentMCP` takes the owner's hooks, and the subagent tool hooks its nested copy when built;
  the child pool holds the hooked `profile_message` copy).
- A child's session MCP call fires the owner's pre_tool and post_tool once each, and a blocking pre_tool
  stops it (`SubagentMCP.BindToolsets` hooks each child's session copy after the gate).
- A denied tool stays out of the tools array; a lazy entry that the profile hides is a config error.
- A child of a parent with lazy MCP tools gates its copy with the owner's set: an unloaded lazy MCP tool
  is refused with `tool_not_loaded`, and the server does not run.
- A subagent child calls `load_tool` only when its owner has `tools.native.load_tool` and `tools.lazy`
  and the agent's tools list names it; its loads go to the owner's `lazy_tools.json`. A child of an owner
  without `tools.lazy` gets no `load_tool`, and its call is refused as `unknown_tool`.
- A native lazy tool is gated for a child as well: a child's call to an unloaded one is refused with
  `tool_not_loaded`, and the real schema is not run.
- `load_tool` is absent unless `tools.native.load_tool` is enabled, and the app never adds it.
- `tools.lazy` without `tools.native.load_tool` is refused at load, and the error names both keys.
- `tools.native.load_tool` with no `tools.lazy` is listed in the tools array and lists "No lazy tools.".
  The key alone decides: with no key, `load_tool` is absent.
- `tools.native.load_tool` takes no keys, and `load_tool` is in the supported-keys error text.
- A pattern over a server with more than `tools.lazy_load_max` lazy tools is refused with nothing
  loaded; a narrower selection within the limit loads and writes the file once.
- Fingerprint change clears the set; a corrupt file fails closed; a loaded ID that no longer
  exists does not break the run; a wildcard that gains a tool refuses the new tool.

## Not in v1

- No dispatcher: a lazy tool is called by its own name.
- No next-round expansion beyond the next request: a load is listed from the next request, not in the
  round that made it.
- A subagent child (parent-profile and remote) uses its owning profile's `tools.lazy` set and state
  folder: its MCP tools are gated with that set, and its loads go to the owning profile's `lazy_tools.json`.
  `load_tool` reaches a child only when the owning profile enables `tools.native.load_tool` and the
  agent's tools list names it; an owner without `tools.lazy` gives its children no gating and no `load_tool`.
- A profile_message target runs with its own state folder (`<state>/<session>/<category>/<name>`) and its
  own `tools.lazy`. Its loads go to its own `lazy_tools.json`, and the caller's loads go to the caller's
  file; a target without `tools.lazy` gets no `load_tool` and no file. Tested by the profile_message lazy tests.

## Verification limits

- Accepted verification exception: the resolver maps a child `load_tool` catalog error to a
  `ConfigError` with the settings path (`internal/app/resolver.go`, the `SetLazy` call in
  `buildRegisteredCatalog`).
- No test reaches that mapping. No config input produces the error, and the resolver's `SetLazy`
  cannot see a duplicate because `newSubagentToolRemotes` rebinds the hook first
  (`internal/tools/native/catalog.go:187`, `internal/tools/native/subagent.go:304`).
- The error is covered at the `SetLazy` boundary by `TestSetLazyReturnsTheCatalogErrorForADuplicateLoadTool`.
  The wrap itself is verified by reading only.
- Accepted by the user as a verification exception.
