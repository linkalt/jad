# Agent Tool Design Guidelines

Design tools specifically for use by an AI agent.

Tools are **not human-facing APIs**.

Do not optimize schemas for human readability, explicitness, documentation completeness, or conventional API design if doing so increases model effort, token usage, validation failures, or retries.

Optimize tools for:

```text
minimal arguments
clear intent
safe backend defaults
low token usage
few validation failures
few retries
few provider round trips
predictable execution
compact results
```

The preferred interaction is:

```text
agent expresses intent
→ backend normalizes arguments
→ backend applies safe defaults
→ tool executes
→ compact result returns
```

Avoid designs that require:

```text
agent predicts backend details
→ agent fills boilerplate arguments
→ strict validation rejects minor omissions
→ model retries
→ additional provider round trip
```

---

# Core Principle

A tool schema should expose only the information the agent actually needs to decide.

Implementation details, predictable defaults, backend policy, and routine configuration should normally remain in the backend.

Ask for every parameter:

```text
Does the agent actually need to decide this value?
```

If not, do not force the agent to provide it.

Prefer:

```ts
read({
  path
})
```

over:

```ts
read({
  path,
  offset: 0,
  limit: 200,
  encoding: "utf8",
  format: "text"
})
```

when those additional values are predictable backend defaults.

---

# Design for Agents, Not Humans

Human-oriented APIs often favor explicit configuration.

Agent-oriented tools should favor minimal intent.

A human API may reasonably require:

```ts
search({
  query,
  limit,
  sort,
  recursive,
  caseSensitive,
  includeHidden,
  outputFormat
})
```

An agent tool should usually allow:

```ts
search({
  query
})
```

and let the backend determine normal defaults.

The agent should specify extra controls only when they materially affect the requested operation.

Do not require the model to repeatedly emit values that the backend already knows.

---

# Keep Schemas Small

Every exposed parameter creates:

```text
more schema tokens
more decision branches
more opportunities for mistakes
more validation paths
more arguments in tool calls
```

Expose a parameter only when it provides meaningful control.

Avoid parameters that:

* have only one realistic value
* are normally constant
* are determined by backend policy
* can be derived from another field
* represent implementation details
* duplicate another argument
* exist only for schema completeness
* the agent almost never needs to change

Keep advanced controls available when useful, but optional.

Prefer:

```ts
type ReadArgs = {
  path: string;
  offset?: number;
  limit?: number;
};
```

instead of forcing all three fields.

---

# Required Parameters

Use `required` sparingly.

A parameter should be required only when:

```text
the operation cannot safely or meaningfully execute without it
```

Not because:

```text
the tool normally uses it
```

For every parameter, ask:

```text
Can the backend infer this?

Can the backend safely default this?

Can the tool still perform a useful operation without it?

Would omitting it make the request genuinely ambiguous?

Would guessing it materially change or endanger the operation?
```

If the backend can safely recover, make the parameter optional.

---

# Backend Defaults

Move predictable behavior into the backend.

Good candidates include:

* offset
* limit
* page size
* context lines
* recursion depth
* recursive flags
* sorting mode
* result count
* encoding
* output format
* safe timeout
* optional filters
* case sensitivity
* pagination position

Example:

```ts
function read(args: ReadArgs) {
  const offset = args.offset ?? 0;
  const limit = args.limit ?? DEFAULT_READ_LIMIT;

  // execute
}
```

The agent should not spend tokens sending predictable defaults.

Do not make everything optional simply for minimalism.

Defaults must remain:

```text
safe
predictable
non-surprising
consistent
```

---

# Prefer Intent Over Configuration

Tool arguments should describe **what the agent wants**, not how the implementation should internally accomplish it.

Prefer:

```ts
find({
  query: "createSession"
})
```

over requiring implementation controls such as:

```ts
find({
  query: "createSession",
  engine: "regex-v2",
  bufferSize: 4096,
  traversalMode: "filesystem",
  resultSerializer: "compact"
})
```

Backend implementation decisions should stay internal unless the agent genuinely benefits from controlling them.

---

# Tolerant Validation

Validation exists to prevent unsafe, invalid, contradictory, or genuinely ambiguous operations.

It should not enforce unnecessary ceremony.

Prefer:

```text
normalize
→ default
→ execute
```

instead of:

```text
reject
→ schema error
→ model retry
→ extra provider request
```

For minor compatible differences, normalize inputs where safe.

Examples:

```text
missing optional boolean
→ apply default

missing offset
→ use 0

missing limit
→ use backend default

case-insensitive enum equivalent
→ normalize

empty optional filter
→ treat as absent

value slightly outside harmless range
→ clamp when semantics remain obvious
```

Do not aggressively guess values when the guess could materially change the operation.

---

# Recoverable vs Hard Errors

Design tool execution around two classes of problems.

## Recoverable Conditions

Handle internally when possible.

Examples:

* optional parameter missing
* pagination omitted
* empty optional filter
* harmless capitalization differences
* optional output mode omitted
* limit outside a safe soft range
* encoding omitted when a default exists

Preferred behavior:

```text
normalize
→ execute
→ optionally return compact metadata
```

Example:

```json
{
  "status": "success",
  "defaulted": {
    "limit": 200
  }
}
```

If default behavior is normal and obvious, no warning is necessary.

## Hard Errors

Return an error when:

* the target cannot be identified
* execution would be unsafe
* required information cannot be inferred
* arguments contradict each other
* the requested action is impossible
* continuing risks destructive behavior
* the tool cannot determine what operation was intended

Hard errors should represent actual execution blockers.

---

# Destructive and High-Impact Tools

Minimalism must not weaken safety.

Tools that modify external state require enough information to identify the intended action precisely.

Examples:

* delete
* overwrite
* send
* publish
* deploy
* terminate
* kill
* modify permissions
* execute financial actions
* mutate remote resources

For these operations, require parameters necessary to identify:

```text
what
where
which target
which action
```

Do not make critical targeting information optional merely to reduce tokens.

The rule is:

```text
minimal where safe
strict where necessary
```

---

# Preserve Advanced Controls

Minimal common calls should not remove useful precision.

A good design supports both:

```ts
read({
  path: "src/index.ts"
})
```

and:

```ts
read({
  path: "src/index.ts",
  offset: 500,
  limit: 100
})
```

The common case stays small while advanced controls remain available.

The goal is not:

```text
remove configurability
```

The goal is:

```text
do not force configurability into every call
```

---

# Avoid Redundant Parameters

Do not expose multiple fields expressing the same intent unless there is a strong reason.

Avoid designs like:

```ts
{
  path,
  filePath,
  target,
  targetPath
}
```

or:

```ts
{
  recursive,
  depth
}
```

when one can define the behavior.

Prefer one canonical representation.

If backward-compatible aliases must exist, normalize them internally rather than forcing the model to understand multiple equivalent forms.

---

# Avoid Mode Fields When Structure Already Expresses Intent

Be cautious with parameters such as:

```ts
mode: "read"
type: "file"
action: "search"
operation: "find"
```

when the tool itself already identifies the operation.

For a tool named:

```text
read
```

this is usually redundant:

```ts
read({
  mode: "read",
  path
})
```

Prefer:

```ts
read({
  path
})
```

Do not make the agent restate information already represented by the selected tool.

---

# Prefer Flat Schemas

Avoid deep nesting unless the structure provides real value.

Prefer:

```ts
{
  path,
  offset?,
  limit?
}
```

over:

```ts
{
  target: {
    file: {
      path
    }
  },
  options: {
    pagination: {
      offset,
      limit
    }
  }
}
```

Deep schemas cost tokens and increase generation mistakes.

Use nested objects when fields genuinely belong to reusable or mutually exclusive structures, not merely for organizational neatness.

---

# Avoid Schema Purity

Do not optimize for theoretical API elegance.

The goal is reliable agent execution.

A partially specified call should succeed when enough information exists.

This:

```ts
read({
  path: "src/index.ts"
})
```

should not fail merely because `offset` and `limit` were not explicitly supplied.

Agent tools are operational interfaces, not exercises in explicit configuration.

---

# Keep Tool Descriptions Efficient

Descriptions should be clear enough for the model to select and use the tool correctly, but not verbose.

A tool description should primarily explain:

```text
what the tool does
when to use it
important behavioral constraints
critical argument semantics
```

Avoid documenting obvious implementation details.

Avoid repeating the same information in:

```text
tool description
parameter description
enum description
examples
warnings
```

unless repetition materially improves tool selection.

Schema tokens are repeated in model context, so unnecessary documentation has ongoing cost.

---

# Parameter Descriptions

Describe parameters based on distinctions the agent needs to understand.

Good:

```text
path:
File or directory to read.
```

Bad:

```text
path:
A UTF-8 encoded string representing the absolute or relative filesystem path
that identifies the file-system entity which will be passed into the internal
filesystem resolution subsystem...
```

Descriptions should resolve ambiguity, not document implementation internals.

---

# Tool Naming

Choose names that communicate the action immediately.

Prefer:

```text
read
write
edit
search
find
glob
exec
delete
move
copy
```

Avoid unnecessarily verbose names such as:

```text
performFilesystemReadOperation
executeAdvancedContentSearch
```

Short tool names reduce tokens and are easier for models to distinguish.

Do not make names so short or generic that different tools become ambiguous.

---

# Tool Granularity

A tool should represent a coherent agent action.

Avoid both extremes:

```text
one giant universal tool with dozens of modes
```

and:

```text
many tiny tools differing only by trivial options
```

Prefer tools with clear semantic boundaries.

For example:

```text
read
search
edit
exec
```

is generally better than:

```text
filesystem_operation({
  action: ...
})
```

with twenty unrelated actions hidden behind a mode field.

However, do not split tools merely for conceptual purity if it increases selection complexity without improving reliability.

---

# Tool Selection Should Be Obvious

The agent should not need extensive reasoning to decide between nearly identical tools.

Avoid overlapping tools such as:

```text
search_text
find_text
grep_text
query_text
lookup_text
```

unless they have meaningfully different capabilities.

When two tools overlap heavily:

```text
merge them
```

or:

```text
make their intended use clearly distinct
```

Ambiguous tool selection causes retries and wasted reasoning.

---

# Design for Partial Agent Knowledge

The agent will not always know every possible argument value.

Tools should allow useful operations from incomplete but sufficient intent.

For example:

```ts
search({
  query: "SessionManager"
})
```

can reasonably search from the current/default workspace.

Do not require:

```ts
search({
  query: "SessionManager",
  root: "/workspace/project",
  recursive: true,
  maxResults: 100,
  caseSensitive: false
})
```

if the backend already has workspace context and normal defaults.

---

# Backend Context Is Valuable

If the backend already knows something reliably, avoid asking the agent to repeat it.

Examples:

* active workspace
* current repository
* current session
* default project root
* provider context
* authenticated account
* normal result limit
* safe timeout policy
* default encoding

Do not force repeated context through tool arguments unless the agent needs to override it.

---

# Normalize Before Execution

A typical implementation should follow:

```ts
function execute(rawArgs: RawArgs) {
  const args = normalize(rawArgs);
  validateCriticalFields(args);
  return performOperation(args);
}
```

Normalization may include:

```text
aliases
defaults
case normalization
empty-value handling
safe clamping
derived arguments
canonical paths
```

Critical validation should happen after harmless normalization where appropriate.

---

# TypeScript Design

Keep all layers consistent.

The following must agree:

```text
TypeScript argument type
runtime validator
generated/provider schema
dispatcher
execution logic
tests
```

Avoid this mismatch:

```ts
type ReadArgs = {
  path: string;
  offset?: number;
  limit?: number;
};
```

while runtime code does:

```ts
if (offset === undefined || limit === undefined) {
  throw new Error("offset and limit required");
}
```

Also avoid:

```ts
type ReadArgs = {
  path: string;
  offset: number;
};
```

when execution already supports:

```ts
const offset = args.offset ?? 0;
```

If the backend supports omission, the public agent-facing schema should normally support omission too.

---

# Provider Schemas

When tool definitions are converted for multiple providers, preserve the same semantic behavior.

Ensure optional fields do not accidentally become required in:

```text
OpenAI schemas
Anthropic schemas
Gemini schemas
JSON Schema generation
runtime validators
internal adapters
```

The model-facing schema and backend behavior must agree.

Do not make one provider artificially stricter simply because of adapter implementation.

---

# Result Design

Tool results become model input.

Keep them compact.

Return information the agent needs for the next decision.

Avoid:

* explanatory prose
* duplicated fields
* large metadata blocks
* implementation diagnostics during normal success
* repeated request arguments
* verbose descriptions of defaults

Prefer:

```json
{
  "status": "success",
  "content": "...",
  "nextOffset": 200
}
```

over large response envelopes.

---

# Result Metadata

Include metadata only when operationally useful.

Useful examples:

```text
truncated
nextOffset
hasMore
matchedCount
exitCode
changed
path
defaulted
warning
```

Avoid returning values merely because they are available internally.

Every result field adds context tokens.

---

# Warnings

Warnings should be:

```text
compact
non-blocking
actionable
rare
```

Do not warn for normal expected default behavior.

For example, if omitting `limit` is the intended common usage:

```text
do not return:
warning: limit was omitted and defaulted to 200
```

on every request.

Warnings are useful when fallback behavior is unusual or may affect interpretation.

---

# Pagination

Do not require pagination parameters for the first call.

Prefer:

```ts
search({
  query
})
```

with a bounded backend default.

Then return something compact such as:

```json
{
  "results": [...],
  "nextOffset": 100,
  "hasMore": true
}
```

The agent can request additional data only when needed.

This reduces unnecessary output and tool-call arguments.

---

# Output Limits

Tools should protect the model context automatically.

Do not rely on the agent to always specify safe output limits.

The backend should enforce reasonable limits for:

```text
file reads
search results
command output
logs
directory listings
database results
```

Expose override controls when useful, but keep safe limits as backend policy.

---

# Error Messages

Errors are also agent input.

Make them concise and operational.

Good:

```text
error: path not found
path: src/foo.ts
```

Better when recovery is known:

```text
error: path not found
path: src/foo.ts
suggestion: nearest match is src/foo/index.ts
```

Avoid long human-facing explanations.

The agent needs information for the next action, not customer-support prose.

---

# Avoid Forced Retry Loops

Before returning a validation error, ask:

```text
Can the backend safely repair this call?
```

If yes, repair it.

Every rejected tool call may create another:

```text
model inference
tool call
tool response
```

A small amount of deterministic backend normalization is usually cheaper and more reliable.

---

# Deterministic Backend Logic Over Model Decisions

If a value can be determined reliably using deterministic code, prefer doing it in the backend rather than asking the model.

Examples:

```text
default pagination
path normalization
safe maximum output
default encoding
workspace root
sorting defaults
timeout policy
empty-filter removal
```

Do not spend model reasoning on deterministic implementation details.

---

# Do Not Over-Generalize

Minimal APIs should still preserve semantic clarity.

Do not turn every tool into:

```ts
tool({
  input: string
})
```

just to reduce schema size.

The agent still benefits from structured arguments when they represent meaningful distinctions.

Good tool design minimizes **unnecessary structure**, not all structure.

---

# Creation Process

When designing a tool:

```text
1. Define the agent intent the tool represents.
2. Identify the minimum information required to perform that intent.
3. Separate critical information from optional control.
4. Move predictable behavior into backend defaults.
5. Expose advanced controls only when useful.
6. Design tolerant normalization.
7. Define true hard-error conditions.
8. Add safety requirements for destructive behavior.
9. Keep the schema flat and compact where possible.
10. Keep the result compact and operational.
11. Ensure TypeScript, validation, provider schema, and execution agree.
12. Test the smallest valid call first.
```

---

# Smallest Valid Call

Every tool should have an intentional smallest useful invocation.

Examples:

```ts
read({
  path
})
```

```ts
search({
  query
})
```

```ts
glob({
  pattern
})
```

```ts
exec({
  command
})
```

Design and test this path explicitly.

If the smallest normal call contains many predictable arguments, reconsider whether those arguments belong in the agent-facing schema.

---

# Test Minimal Calls

Tests should include minimal input, not just fully specified input.

For example:

```ts
await read({
  path: "src/index.ts"
});
```

Verify that:

```text
defaults are applied
execution succeeds
results stay bounded
no unnecessary warning is returned
```

Also test explicit overrides separately.

---

# Safety Overrides Efficiency

When efficiency conflicts with correctness or safety:

```text
correctness and safety win
```

This is especially important for:

```text
destructive operations
external side effects
security-sensitive operations
authentication
permissions
deployment
publishing
sending
financial actions
```

Do not infer critical intent merely to avoid another tool call.

---

# Avoid Premature Features

When creating tools, do not add parameters for hypothetical future use.

Avoid:

```text
maybe we will need this later
```

Every field has an ongoing context and decision cost.

Add controls when there is a concrete agent use case.

---

# Final Design Criteria

A well-designed agent tool should satisfy:

* the smallest common call is genuinely small
* only truly critical fields are required
* predictable values are backend defaults
* schemas expose agent decisions, not backend implementation details
* minor omissions do not trigger unnecessary retries
* compatible inputs are normalized when safe
* validation rejects only meaningful problems
* destructive operations remain precise and strict
* advanced controls remain available but optional
* tool selection is obvious
* overlapping tools are minimized
* schemas are shallow and compact
* descriptions are concise but unambiguous
* TypeScript and runtime validation agree
* provider schemas preserve the same semantics
* outputs are bounded automatically
* results contain only useful next-step information
* warnings are rare and compact
* errors help the agent recover
* normal usage requires minimal reasoning and minimal tokens

The final system should optimize for:

```text
agent intent
→ minimal call
→ deterministic backend handling
→ reliable execution
→ compact result
```

The primary objective is not API completeness.

It is:

```text
maximum agent reliability
with minimum model effort,
minimum tokens,
minimum retries,
and minimum provider round trips.
```
