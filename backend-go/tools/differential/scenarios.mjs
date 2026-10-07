import {createHash} from "node:crypto";
import {mkdir, readFile, readdir, rm, writeFile} from "node:fs/promises";
import {dirname, join} from "node:path";

// Each scenario returns a JSON-serializable record of what a client observes.
async function call(ctx, method, path, {body, headers = {}} = {}) {
  const response = await fetch(`${ctx.base}${path}`, {
    method,
    headers: {authorization: `Bearer ${ctx.token}`, ...(body === undefined ? {} : {"content-type": "application/json"}), ...headers},
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const raw = await response.text();
  const contentType = (response.headers.get("content-type") ?? "").split(";")[0];
  let parsed = raw;
  try { parsed = JSON.parse(raw); } catch {}
  // An event stream is compared as events so object key order inside the raw
  // text does not count as a difference.
  if (contentType === "text/event-stream") parsed = parseEvents(raw);
  return {status: response.status, contentType, body: parsed};
}

function parseEvents(raw) {
  return raw.split("\n\n").filter((block) => block && !block.startsWith(":")).map((block) => {
    const event = /^event: (.*)$/m.exec(block)?.[1] ?? "message";
    const data = block.split("\n").filter((l) => l.startsWith("data:")).map((l) => l.slice(5).trim()).join("\n");
    let parsed = data;
    try { parsed = JSON.parse(data); } catch {}
    return {event, data: parsed};
  }).filter((e) => !(e.event === "ui.delta" && e.data?.append === ""));
}

// Go keeps a lock file next to the files it guards for cross-process safety;
// the TypeScript backend has no such file, so it is not part of the comparison.
const listSessionFiles = async (dir) => (await readdir(dir)).filter((f) => !f.endsWith(".jad-lock")).sort();

async function sse(ctx, path, {method = "GET", body} = {}) {
  const response = await fetch(`${ctx.base}${path}`, {
    method,
    headers: {authorization: `Bearer ${ctx.token}`, ...(body === undefined ? {} : {"content-type": "application/json"})},
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const raw = await response.text();
  const events = raw.split("\n\n").filter((block) => block && !block.startsWith(":")).map((block) => {
    const event = /^event: (.*)$/m.exec(block)?.[1] ?? "message";
    const data = block.split("\n").filter((l) => l.startsWith("data:")).map((l) => l.slice(5).trim()).join("\n");
    let parsed = data;
    try { parsed = JSON.parse(data); } catch {}
    return {event, data: parsed};
  });
  return {status: response.status, contentType: (response.headers.get("content-type") ?? "").split(";")[0], events: events.filter(meaningful)};
}

// The TypeScript run rebuilds a finished tool-call part as a new object with the
// same content and, comparing by identity, publishes an empty `ui.delta` for it.
// Go compares parts by value and sends nothing. An empty append changes no client
// state, so it is excluded rather than reproduced.
const meaningful = (event) => !(event.event === "ui.delta" && event.data?.append === "");

const eventNames = (result) => result.events.map((e) => e.event);

// Reads an SSE response incrementally so a scenario can react to an event (for
// example answer a pending question) while the run is still going.
async function sseLive(ctx, path, {body, onEvent}) {
  const response = await fetch(`${ctx.base}${path}`, {
    method: "POST",
    headers: {authorization: `Bearer ${ctx.token}`, "content-type": "application/json"},
    body: JSON.stringify(body),
  });
  const events = [];
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const {done, value} = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, {stream: true});
    let index;
    while ((index = buffer.indexOf("\n\n")) >= 0) {
      const block = buffer.slice(0, index);
      buffer = buffer.slice(index + 2);
      // A comment frame (": keep-alive") carries no event.
      if (!block.trim() || block.startsWith(":")) continue;
      const event = /^event: (.*)$/m.exec(block)?.[1] ?? "message";
      const data = block.split("\n").filter((l) => l.startsWith("data:")).map((l) => l.slice(5).trim()).join("\n");
      let parsed = data;
      try { parsed = JSON.parse(data); } catch {}
      const item = {event, data: parsed};
      if (!meaningful(item)) continue;
      events.push(item);
      await onEvent?.(item);
    }
  }
  return {status: response.status, events};
}

// Lists files under a directory whose name matches, relative to it, so a
// scenario can see whether a backend keeps a partial-response file.
async function findFiles(dir, pattern, prefix = "") {
  const found = [];
  for (const entry of await readdir(dir, {withFileTypes: true}).catch(() => [])) {
    const relative = prefix === "" ? entry.name : `${prefix}/${entry.name}`;
    if (entry.isDirectory()) found.push(...(await findFiles(join(dir, entry.name), pattern, relative)));
    else if (pattern.test(entry.name)) found.push(relative.replace(/[0-9a-f]{8}-[0-9a-f-]{27}/g, "<uuid>"));
  }
  return found.sort();
}

const messagesJsonl = async (ctx, id) =>
  (await readFile(join(ctx.sessions, id, "messages.jsonl"), "utf8")).split("\n").filter(Boolean).map((line) => JSON.parse(line));

// The ETag value embeds the transcript generation (inode, birth time), so only
// its shape is comparable between two server processes.
const etagShape = (value) => (value ?? null) && value.replace(/[0-9a-f]{6,}/gi, "X").replace(/\d+/g, "N");

const toolRound = (name, args) => ({call: {name, arguments: args}});

// The exposed MCP tool name is derived from the server id and tool name, so the
// scenarios compute it the same way the backends do.
const mcpName = (server, tool) => {
  const slug = (value) => value.toLowerCase().replace(/[^a-z0-9_-]+/g, "_").replace(/^_+|_+$/g, "") || "tool";
  const digest = createHash("sha256").update(`${server}\0${tool}`).digest("hex").slice(0, 10);
  return `${`mcp_${slug(server)}_${slug(tool)}`.slice(0, 53)}_${digest}`;
};

// Every function_call_output the provider received in a request, in order.
const toolOutputs = (request) => (request?.input ?? []).filter((item) => item.type === "function_call_output").map((item) => item.output);

// Runs a scripted sequence of tool calls in the `toolbox` session and returns
// what the model saw: every function_call_output, in order.
function toolboxRun(name, steps, extra = async () => ({})) {
  return {
    name,
    provider: {steps: [...steps, {text: "finished"}]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/toolbox/reset", {body: {}});
      const result = await sse(c, "/api/sessions/toolbox/run", {method: "POST", body: {input: name}});
      return {names: eventNames(result), outputs: toolOutputs(c.provider.requests.at(-1)), ...(await extra(c))};
    },
  };
}

// The text of the n-th tool output the provider has been shown, used to read an
// id a tool generated (task ids, memory refs, process ids).
const generated = (requests, index, pattern) => {
  const outputs = toolOutputs(requests.at(-1));
  return String(outputs[index] ?? "").match(pattern)?.[1] ?? "missing";
};

function toolboxScenarios() {
  return [
    toolboxRun("toolbox: grep", [
      toolRound("grep", {pattern: "TODO"}),
      toolRound("grep", {pattern: "TODO", include: "*.ts", context: 1}),
      toolRound("grep", {pattern: "helper(", literal: true}),
      toolRound("grep", {pattern: "nothing-matches-this"}),
      toolRound("grep", {pattern: "TODO", path: ".secret"}),
      toolRound("grep", {pattern: "needle", path: "long.txt"}),
      toolRound("grep", {pattern: "("}),
      toolRound("grep", {pattern: "x", path: "no/such/dir"}),
      toolRound("grep", {pattern: "x", path: "../outside"}),
      toolRound("grep", {pattern: ""}),
      toolRound("grep", {pattern: "TODO", context: 99}),
      toolRound("grep", {pattern: "TODO", exclude: "*.md"}),
      toolRound("grep", {pattern: "TODO", exclude: "!!  lib/**  "}),
      toolRound("grep", {pattern: "TODO", include: "*.ts", exclude: "*.ts"}),
      toolRound("grep", {pattern: "TODO", exclude: "   "}),
      toolRound("grep", {pattern: "TODO", exclude: "!"}),
      toolRound("grep", {pattern: "TODO", exclude: "{docs,src/lib}/**", literal: true}),
    ]),
    toolboxRun("toolbox: glob", [
      toolRound("glob", {}),
      toolRound("glob", {pattern: "*.ts"}),
      toolRound("glob", {pattern: "**/*.{ts,md}"}),
      toolRound("glob", {pattern: "src/**"}),
      toolRound("glob", {pattern: "*", path: "docs"}),
      toolRound("glob", {pattern: "nothing*"}),
      toolRound("glob", {pattern: "*", path: "no/such/dir"}),
      toolRound("glob", {pattern: "*", path: "../outside"}),
      toolRound("glob", {pattern: "caf*"}),
    ]),
    toolboxRun("toolbox: glob unicode and hidden", [
      toolRound("glob", {pattern: "*café*"}),
      toolRound("glob", {pattern: "**/*"}),
      toolRound("glob", {pattern: ".*"}),
      toolRound("glob", {pattern: "{src,docs}/**"}),
    ]),
    toolboxRun("toolbox: memory", [
      toolRound("memory", {operation: "create", type: "note", data: {title: "first", n: 1}}),
      toolRound("memory", {operation: "create", type: "note", data: {title: "second", n: 2}}),
      toolRound("memory", {operation: "get", type: "note", ref: (r) => generated(r, 0, /ref: (\S+)/)}),
      toolRound("memory", {operation: "list", type: "note"}),
      toolRound("memory", {operation: "list", type: "note", order: "oldest", limit: 1}),
      toolRound("memory", {operation: "list", type: "note", filter: {title: "second"}}),
      toolRound("memory", {operation: "update", type: "note", ref: (r) => generated(r, 0, /ref: (\S+)/), data: {title: "first", n: 10}, if_match: "stale"}),
      toolRound("memory", {operation: "update", type: "note", ref: (r) => generated(r, 0, /ref: (\S+)/), data: {title: "first", n: 10}}),
      toolRound("memory", {operation: "get", type: "note", ref: "note/does-not-exist.json"}),
      toolRound("memory", {operation: "create", type: "note"}),
      toolRound("memory", {operation: "create", type: "../escape", data: {}}),
      toolRound("memory", {operation: "frobnicate", type: "note"}),
      toolRound("memory", {operation: "list", type: "note", cursor: "bogus"}),
    ]),
    toolboxRun("toolbox: task", [
      toolRound("task", {action: "add", title: "write tests", priority: "high", tags: ["a", "b"], documents: ["docs/guide.md"]}),
      toolRound("task", {action: "add", title: "ship it"}),
      toolRound("task", {action: "list"}),
      toolRound("task", {action: "get", id: (r) => generated(r, 0, /task_id: (\S+)/)}),
      toolRound("task", {action: "update", id: (r) => generated(r, 0, /task_id: (\S+)/), status: "in-progress"}),
      toolRound("task", {action: "list", status: "in-progress"}),
      toolRound("task", {action: "list", tag: "a", priority: "high"}),
      toolRound("task", {action: "done", id: (r) => generated(r, 1, /task_id: (\S+)/)}),
      toolRound("task", {action: "update", id: (r) => generated(r, 0, /task_id: (\S+)/)}),
      toolRound("task", {action: "get", id: "task-nope"}),
      toolRound("task", {action: "add"}),
      toolRound("task", {title: "implied add"}),
      toolRound("task", {action: "delete", id: (r) => generated(r, 1, /task_id: (\S+)/)}),
      toolRound("task", {action: "list", status: "all", limit: 1}),
    ]),
    toolboxRun("toolbox: process", [
      toolRound("process", {command: "echo started; sleep 0.2; echo finished"}),
      toolRound("process", {action: "wait", id: (r) => generated(r, 0, /process_id: (\S+)/), timeout_ms: 5000}),
      toolRound("process", {action: "status", id: (r) => generated(r, 0, /process_id: (\S+)/)}),
      toolRound("process", {action: "list"}),
      toolRound("process", {command: "sleep 30"}),
      toolRound("process", {action: "stop", id: (r) => generated(r, 4, /process_id: (\S+)/)}),
      toolRound("process", {action: "list", filter: "stopped"}),
      toolRound("process", {command: "echo oops >&2; exit 4"}),
      toolRound("process", {action: "wait", id: (r) => generated(r, 7, /process_id: (\S+)/), timeout_ms: 5000}),
      toolRound("process", {action: "status", id: "proc-nope"}),
      toolRound("process", {action: "start"}),
      toolRound("process", {command: "pwd", cwd: "no/such/dir"}),
    ], async (c) => ({processDir: (await readdir(join(c.root, "toolbox", ".jad"), {recursive: true}).catch(() => [])).length > 0})),
    toolboxRun("toolbox: fetch refusals", [
      toolRound("fetch", {url: "http://127.0.0.1:1/"}),
      toolRound("fetch", {url: "ftp://example.com/file"}),
      toolRound("fetch", {url: "not a url"}),
      toolRound("fetch", {cursor: "bogus"}),
      toolRound("fetch", {}),
      toolRound("fetch", {url: "http://localhost/x", limit: 0}),
      toolRound("fetch", {url: "http://user:pw@example.com/"}),
    ]),
    {
      name: "process env_file: file below inline env, denial last",
      provider: {steps: [
        toolRound("process", {command: "printf '%s|%s|%s|%s|%s' \"$FILE_VALUE\" \"$OVERRIDDEN\" \"$QUOTED\" \"${DENIED-unset}\" \"$NO_COLOR\""}),
        toolRound("process", {action: "wait", id: (r) => generated(r, 0, /process_id: (\S+)/), timeout_ms: 5000}),
        {text: "finished"},
      ]},
      run: async (c) => {
        await call(c, "POST", "/api/sessions/procenv/reset", {body: {}});
        const result = await sse(c, "/api/sessions/procenv/run", {method: "POST", body: {input: "env file"}});
        return {names: eventNames(result), outputs: toolOutputs(c.provider.requests.at(-1))};
      },
    },
    {
      name: "process env_file: missing file is a configuration error",
      provider: {text: "unused"},
      run: async (c) => {
        await call(c, "POST", "/api/sessions/procenvbad/reset", {body: {}});
        return {
          run: await sse(c, "/api/sessions/procenvbad/run", {method: "POST", body: {input: "env file"}}),
          snapshot: await call(c, "GET", "/api/sessions/procenvbad"),
          providerRequests: c.provider.requests.length,
        };
      },
    },
    // A successful fetch needs a host that resolves publicly (the tool refuses
    // loopback and private addresses), so it only runs with DIFF_NETWORK=1.
    ...(process.env.DIFF_NETWORK === "1" ? [toolboxRun("toolbox: fetch success (network)", [
      toolRound("fetch", {url: "https://example.com/"}),
      toolRound("fetch", {url: "https://example.com/", limit: 50}),
      toolRound("fetch", {cursor: (r) => generated(r, 1, /next_cursor: (\S+)/), limit: 50}),
      toolRound("fetch", {url: "https://example.com/does-not-exist-404"}),
    ])] : []),
    toolboxRun("toolbox: read via toolbox session", [
      toolRound("read", {path: "src/main.ts"}),
      toolRound("read", {path: "src/main.ts", offset: 2, limit: 1}),
      toolRound("read", {path: "docs"}),
      toolRound("read", {path: "../outside"}),
    ]),
  ];
}

// A run against a provider that fails. The client sees only the HTTP status (or
// a generic message), never the provider's response body; the number of
// provider requests shows which failures the SDK retries.
function providerFailureScenarios() {
  const failing = (name, provider) => ({
    name: `provider failure: ${name}`,
    provider,
    run: async (c) => {
      await call(c, "POST", "/api/sessions/provfail/reset", {body: {}});
      const run = await sse(c, "/api/sessions/provfail/run", {method: "POST", body: {input: `hello ${name}`}});
      const text = JSON.stringify(run);
      return {
        run,
        leaksProviderBody: text.includes("SECRET-DETAIL"),
        providerRequests: c.provider.requests.length,
        records: await messagesJsonl(c, "provfail").catch(() => null),
        snapshot: await call(c, "GET", "/api/sessions/provfail"),
        // The session must stay usable after the failure.
        next: await sse(c, "/api/sessions/provfail/run", {method: "POST", body: {input: "after the failure"}}).then((r) => eventNames(r)),
      };
    },
  });
  const fast = {"retry-after-ms": "5"};
  return [
    failing("401 is not retried", {status: 401}),
    failing("400 is not retried", {status: 400}),
    failing("404 is not retried", {status: 404}),
    failing("429 is retried then reported", {status: 429, headers: fast}),
    failing("500 is retried then reported", {status: 500, headers: fast}),
    failing("503 is retried then reported", {status: 503, headers: fast}),
    failing("408 is retried then reported", {status: 408, headers: fast}),
    failing("409 is retried then reported", {status: 409, headers: fast}),
    failing("x-should-retry false overrides a 500", {status: 500, headers: {...fast, "x-should-retry": "false"}}),
    failing("x-should-retry true overrides a 400", {status: 400, headers: {...fast, "x-should-retry": "true"}}),
    failing("a retry can succeed", {steps: [{status: 429, headers: fast}, {status: 500, headers: fast}, {text: "recovered"}]}),
    failing("non-JSON error body", {status: 502, headers: {...fast, "content-type": "text/plain"}, errorBody: "SECRET-DETAIL upstream is down"}),
    failing("connection dropped mid-stream", {deltas: ["partial ", "reply"], disconnectAfter: 1}),
    failing("stream ends without response.completed", {deltas: ["partial ", "reply"], endWithoutCompleted: true}),
    ...providerRetryScenarios(),
    ...runTokenLimitScenarios(),
  ];
}

// provider_retry: how many requests the provider sees, and what the client and the
// transcript show, when a request fails. The waits are short (retry-after-ms or a tiny
// initial delay) so the scenarios stay fast; the counts are what is compared.
function providerRetryScenarios() {
  const quick = {"retry-after-ms": "2"};
  const attempt = (name, session, provider, extra = {}) => ({
    name: `provider retry: ${name}`,
    provider,
    run: async (c) => {
      await call(c, "POST", `/api/sessions/${session}/reset`, {body: {}});
      const run = await sse(c, `/api/sessions/${session}/run`, {method: "POST", body: {input: `hello ${name}`}});
      return {
        names: eventNames(run),
        error: run.events.find((e) => e.event === "error")?.data?.message ?? null,
        providerRequests: c.provider.requests.length,
        records: await messagesJsonl(c, session).catch(() => null),
        next: await sse(c, `/api/sessions/${session}/run`, {method: "POST", body: {input: "after"}}).then((r) => eventNames(r)),
        ...extra,
      };
    },
  });
  return [
    attempt("max_retries 0 makes one request", "retryoff", {status: 429, headers: quick}),
    attempt("max_retries 4 makes five requests", "retrymany", {status: 503, headers: quick}),
    attempt("a retry can succeed after three failures", "retrymany", {steps: [{status: 429, headers: quick}, {status: 500, headers: quick}, {status: 502, headers: quick}, {text: "recovered"}]}),
    attempt("a status that cannot succeed is not retried", "retrymany", {status: 401}),
    attempt("x-should-retry false stops a 500", "retrymany", {status: 500, headers: {...quick, "x-should-retry": "false"}}),
    attempt("max_retries 2 makes three requests", "retryslow", {status: 429, headers: {"retry-after-ms": "1"}}),
    attempt("a Retry-After above max_delay_ms is capped, not waited out", "retryslow", {status: 429, headers: {"retry-after": "3600"}}),
    attempt("a failure after the stream started is not retried", "retrymany", {deltas: ["partial ", "reply"], disconnectAfter: 1}),
    // x-ratelimit-reset: the harness cannot see how long a backend waited, only how many
    // requests it made, so these show both backends read each format without a failure
    // and agree on the counts. The wait itself is covered by the unit tests.
    attempt("a 429 whose x-ratelimit-reset is an hour away is capped, not waited out", "retryslow", {status: 429, headers: {"x-ratelimit-reset": String(Date.now() + 3600000)}}),
    attempt("a 429 whose x-ratelimit-reset is in epoch seconds is capped too", "retryslow", {status: 429, headers: {"x-ratelimit-reset": String(Math.floor(Date.now() / 1000) + 3600)}}),
    attempt("a 429 with an unreadable or past x-ratelimit-reset keeps the backoff", "retryslow", {steps: [{status: 429, headers: {"x-ratelimit-reset": "6m0s"}}, {status: 429, headers: {"x-ratelimit-reset": String(Date.now() - 60000)}}, {text: "recovered"}]}),
    {
      // The provider_retry event: one per wait, then one that clears it. Retry-After-Ms
      // fixes wait_ms, so the whole payload is compared except resume_at, which is a
      // clock reading the normalizer masks.
      name: "provider retry: each wait is announced and the notice is cleared",
      provider: {steps: [{status: 429, headers: quick}, {status: 503, headers: quick}, {text: "recovered"}]},
      run: async (c) => {
        await call(c, "POST", "/api/sessions/retrymany/reset", {body: {}});
        const run = await sse(c, "/api/sessions/retrymany/run", {method: "POST", body: {input: "hello announce"}});
        const notices = run.events.filter((e) => e.event === "provider_retry").map((e) => e.data);
        return {
          names: eventNames(run),
          notices,
          turnMatches: notices.every((n) => n.turn_id === run.events.find((e) => e.event === "run")?.data?.turn_id),
          clearedBeforeCompleted: eventNames(run).lastIndexOf("provider_retry") < eventNames(run).indexOf("completed"),
          records: await messagesJsonl(c, "retrymany").catch(() => null),
        };
      },
    },
    {
      name: "provider retry: used-up retries clear the notice before the error",
      provider: {status: 503, headers: quick},
      run: async (c) => {
        await call(c, "POST", "/api/sessions/retrymany/reset", {body: {}});
        const run = await sse(c, "/api/sessions/retrymany/run", {method: "POST", body: {input: "hello exhausted"}});
        const names = eventNames(run);
        return {
          names,
          notices: run.events.filter((e) => e.event === "provider_retry").map((e) => e.data),
          clearedBeforeError: names.lastIndexOf("provider_retry") < names.indexOf("error"),
          error: run.events.find((e) => e.event === "error")?.data?.message ?? null,
        };
      },
    },
    {
      name: "provider retry: null max_retries refuses the run",
      run: async (c) => ({run: await call(c, "POST", "/api/sessions/retrybad/run", {body: {input: "go"}}), snapshot: await call(c, "GET", "/api/sessions/retrybad")}),
    },
  ];
}

// max_run_tokens: the fake provider reports total_tokens 19 for every response, so the
// capstop session (cap 30) is under its cap after one round and over it after two. What is
// compared is how many requests the provider sees, the message the client gets, and the
// transcript: every call of the refused round has a result.
function runTokenLimitScenarios() {
  const rounds = [toolRound("read", {path: "README.md"}), toolRound("read", {path: "README.md"}), toolRound("read", {path: "README.md"}), {text: "finished"}];
  const attempt = (name, session) => ({
    name: `max_run_tokens: ${name}`,
    provider: {steps: rounds},
    run: async (c) => {
      await call(c, "POST", `/api/sessions/${session}/reset`, {body: {}});
      const first = await sse(c, `/api/sessions/${session}/run`, {method: "POST", body: {input: `go ${name}`}});
      const afterFirst = c.provider.requests.length;
      const second = await sse(c, `/api/sessions/${session}/run`, {method: "POST", body: {input: "again"}});
      return {
        names: eventNames(first),
        error: first.events.find((e) => e.event === "error")?.data?.message ?? null,
        requestsAfterFirstRun: afterFirst,
        records: await messagesJsonl(c, session),
        secondNames: eventNames(second),
        secondError: second.events.find((e) => e.event === "error")?.data?.message ?? null,
        snapshot: await call(c, "GET", `/api/sessions/${session}`),
      };
    },
  });
  return [
    attempt("a run that reaches the cap stops before the next round", "capstop"),
    attempt("without the key the same run is not capped", "capnone"),
    {
      name: "max_run_tokens: null refuses the run",
      run: async (c) => ({run: await call(c, "POST", "/api/sessions/capbad/run", {body: {input: "go"}}), snapshot: await call(c, "GET", "/api/sessions/capbad")}),
    },
  ];
}

// Bash output beyond the stream budget is spilled to <cwd>/.bash-output. Each
// scenario runs one session with its own spill policy and reports what the model
// saw plus the name, size and digest of every file left behind.
async function spillFiles(c, id) {
  const dir = join(c.root, `${id}-workspace`, ".bash-output");
  let names = [];
  try { names = (await readdir(dir)).sort(); } catch {}
  const files = [];
  for (const name of names) {
    const bytes = await readFile(join(dir, name));
    files.push({name, size: bytes.length, sha256: createHash("sha256").update(bytes).digest("hex")});
  }
  return files;
}

// The TypeScript collector evicts its tail window one read chunk at a time, so
// how many tail bytes survive depends on how the operating system split the
// pipe reads (the same 108894 bytes retained 39742 to 49152 bytes depending on
// chunking). Go keeps an exact rolling tail, so the two cannot agree on the
// omitted count or the start of the tail. What stays deterministic, and is
// compared, is the head, the markers, the final bytes of the stream, and that
// the retained text stays within the window budget.
const TAIL_BUDGET = 16 * 1024 + 32 * 1024 + 4 * 1024 + 12 * 1024 + 512;
function squashOutput(text) {
  if (typeof text !== "string" || !text.includes("bytes omitted")) return text;
  const start = text.indexOf("encoding=utf8 ---\n") + "encoding=utf8 ---\n".length;
  const end = text.lastIndexOf("\n--- END CONTENT");
  const body = text.slice(start, end);
  const within = Buffer.byteLength(body, "utf8") <= TAIL_BUDGET;
  // The tail window starts after the marker; compare only where it ends.
  const marker = body.search(/\[\.\.\. \d+ bytes omitted \.\.\.\]/);
  const head = body.slice(0, marker);
  const tail = body.slice(body.indexOf("]", marker) + 1);
  const summary = `head=${JSON.stringify(head.slice(0, 120))}(${head.length}) tailEnd=${JSON.stringify(tail.slice(-120))} withinBudget=${within}`;
  return `${text.slice(0, start).replace(/omitted_bytes: \d+/, "omitted_bytes: $N")}${summary}${text.slice(end)}`;
}

function spillScenario(name, session, commands) {
  return {
    name,
    provider: {steps: [...commands.map((command) => toolRound("bash", {command})), {text: "finished"}]},
    run: async (c) => {
      // Scenarios share a session workspace; start each one without the spill
      // files an earlier scenario left behind.
      await rm(join(c.root, `${session}-workspace`, ".bash-output"), {recursive: true, force: true});
      await call(c, "POST", `/api/sessions/${session}/reset`, {body: {}});
      const result = await sse(c, `/api/sessions/${session}/run`, {method: "POST", body: {input: name}});
      const records = (await messagesJsonl(c, session)).map((record) => (record.type === "function_call_output" ? {...record, output: squashOutput(record.output)} : record));
      return {names: eventNames(result), outputs: toolOutputs(c.provider.requests.at(-1)).map(squashOutput), files: await spillFiles(c, session), records};
    },
  };
}

const bigStdout = "seq 1 20000";
const hugeStdout = "seq 1 40000";
const bigStderr = "seq 1 20000 >&2";

function spillScenarios() {
  return [
    // Small output needs no file; large output is truncated and its file kept.
    spillScenario("bash spill: small output leaves no file", "spilldefault", ["echo small"]),
    spillScenario("bash spill: large stdout is truncated and kept", "spilldefault", [bigStdout]),
    spillScenario("bash spill: large stderr is truncated and kept", "spilldefault", [bigStderr]),
    spillScenario("bash spill: both streams truncated", "spilldefault", [`${bigStdout}; ${bigStderr}`]),
    spillScenario("bash spill: failing command with large output", "spilldefault", [`${bigStdout}; exit 4`]),
    spillScenario("bash spill: several calls share one directory", "spilldefault", [bigStdout, "echo tiny", bigStderr]),
    spillScenario("bash spill: one very long line", "spilldefault", ["head -c 90000 /dev/zero | tr '\\0' 'a'; echo"]),
    // keep=always retains small outputs too; cap_bytes stops a runaway stream.
    spillScenario("bash spill keep always: small output is kept", "spillalways", ["echo small", "echo oops >&2"]),
    spillScenario("bash spill keep always: output over cap_bytes", "spillalways", [hugeStdout]),
    spillScenario("bash spill keep always: empty output", "spillalways", ["true"]),
    // Disabled spill truncates without writing anything.
    spillScenario("bash spill disabled: large output", "spilloff", [bigStdout]),
  ];
}

// Runs a scripted sequence of tool calls in the named session and returns what
// the model saw, like toolboxRun but for any session.
function sessionToolRun(name, session, steps) {
  return {
    name,
    provider: {steps: [...steps, {text: "finished"}]},
    run: async (c) => {
      await call(c, "POST", `/api/sessions/${session}/reset`, {body: {}});
      const result = await sse(c, `/api/sessions/${session}/run`, {method: "POST", body: {input: name}});
      return {names: eventNames(result), outputs: toolOutputs(c.provider.requests.at(-1))};
    },
  };
}

function skillScenarios() {
  const long = "x".repeat(65);
  return [
    sessionToolRun("skill: list and load", "skilled", [
      toolRound("skill", {}),
      toolRound("skill", {name: "summarize"}),
      toolRound("skill", {name: "only-shared"}),
      toolRound("skill", {name: "accents"}),
    ]),
    sessionToolRun("skill: name variants", "skilled", [
      toolRound("skill", {name: "Agent_Tool_Design"}),
      toolRound("skill", {name: "  agent tool design  "}),
      toolRound("skill", {name: "AGENT-TOOL-DESIGN"}),
      toolRound("skill", {name: "   "}),
    ]),
    sessionToolRun("skill: not found", "skilled", [
      toolRound("skill", {name: "summarise"}),
      toolRound("skill", {name: "mismatched"}),
      toolRound("skill", {name: "other-folder"}),
      toolRound("skill", {name: long}),
    ]),
    sessionToolRun("skill: names cannot escape the directories", "skilled", [
      toolRound("skill", {name: "../outside-skill"}),
      toolRound("skill", {name: ".."}),
      toolRound("skill", {name: "summarize/../../outside-skill"}),
      toolRound("skill", {name: "/etc/passwd"}),
    ]),
    sessionToolRun("skill: invalid arguments", "skilled", [
      toolRound("skill", {name: "summarize", extra: true}),
      toolRound("skill", {name: 7}),
      toolRound("skill", {name: null}),
    ]),
    sessionToolRun("skill: max_bytes cuts a skill and the list", "skilledtiny", [
      toolRound("skill", {name: "accents"}),
      toolRound("skill", {name: "summarize"}),
      toolRound("skill", {}),
    ]),
    {
      name: "skill: an unsupported setting is a configuration error",
      provider: {text: "unused"},
      run: async (c) => {
        await call(c, "POST", "/api/sessions/skillbad/reset", {body: {}});
        return {
          run: await sse(c, "/api/sessions/skillbad/run", {method: "POST", body: {input: "skill"}}),
          snapshot: await call(c, "GET", "/api/sessions/skillbad"),
          providerRequests: c.provider.requests.length,
        };
      },
    },
  ];
}

export const scenarios = [
  {name: "health", run: (c) => call(c, "GET", "/api/health")},
  {name: "unauthorized", run: async (c) => ({status: (await fetch(`${c.base}/api/sessions`)).status})},
  {name: "not found", run: (c) => call(c, "GET", "/api/nothing")},
  {name: "session list", run: (c) => call(c, "GET", "/api/sessions")},
  {name: "session snapshot empty", run: (c) => call(c, "GET", "/api/sessions/alpha")},
  {name: "session snapshot unknown", run: (c) => call(c, "GET", "/api/sessions/missing")},
  {name: "session snapshot bad reference", run: (c) => call(c, "GET", "/api/sessions/..%2Fx")},
  {name: "history empty", run: (c) => call(c, "GET", "/api/sessions/alpha/history")},
  {name: "files", run: (c) => call(c, "GET", "/api/sessions/alpha/files")},
  {name: "tools", run: (c) => call(c, "GET", "/api/sessions/alpha/tools")},
  {name: "profiled snapshot", run: (c) => call(c, "GET", "/api/sessions/profiled")},
  {name: "profiled other profile", run: (c) => call(c, "GET", "/api/sessions/profiled?profile=two")},
  {name: "profiled unknown profile", run: (c) => call(c, "GET", "/api/sessions/profiled?profile=nope")},
  {name: "run empty body", run: (c) => call(c, "POST", "/api/sessions/alpha/run", {body: {}})},
  {
    name: "run streams a reply",
    provider: {deltas: ["hello ", "world"]},
    run: async (c) => {
      const result = await sse(c, "/api/sessions/alpha/run", {method: "POST", body: {input: "hi there"}});
      const snapshot = await call(c, "GET", "/api/sessions/alpha");
      const history = await call(c, "GET", "/api/sessions/alpha/history");
      return {result, snapshot, history, providerRequests: c.provider.requests.length};
    },
  },
  {
    name: "second turn keeps context",
    provider: {text: "second"},
    run: async (c) => {
      const result = await sse(c, "/api/sessions/alpha/run", {method: "POST", body: {input: "again"}});
      const snapshot = await call(c, "GET", "/api/sessions/alpha");
      return {names: eventNames(result), snapshot, lastRequestInputItems: c.provider.requests.at(-1)?.input?.length};
    },
  },
  {
    name: "conditional snapshot",
    run: async (c) => {
      const first = await fetch(`${c.base}/api/sessions/alpha`, {headers: {authorization: `Bearer ${c.token}`}});
      const etag = first.headers.get("etag");
      const second = await fetch(`${c.base}/api/sessions/alpha`, {headers: {authorization: `Bearer ${c.token}`, "if-none-match": etag ?? ""}});
      return {firstStatus: first.status, hasEtag: Boolean(etag), secondStatus: second.status};
    },
  },
  {name: "reset", run: async (c) => ({reset: await call(c, "POST", "/api/sessions/alpha/reset", {body: {}}), after: await call(c, "GET", "/api/sessions/alpha")})},
  {name: "compact empty", run: (c) => call(c, "POST", "/api/sessions/beta/compact", {body: {}})},
  {name: "profile switch", run: (c) => call(c, "POST", "/api/sessions/profiled/profile", {body: {active_profile: "two"}})},
  {name: "profile switch invalid", run: (c) => call(c, "POST", "/api/sessions/profiled/profile", {body: {active_profile: "nope"}})},
  {name: "model default", run: (c) => call(c, "POST", "/api/sessions/profiled/model-default", {body: {model_default: "alternate.json"}, profile: "two"})},
  {name: "run stop without run", run: (c) => call(c, "POST", "/api/sessions/beta/run/stop", {body: {}})},
  {name: "run stream without run", run: (c) => call(c, "GET", "/api/sessions/beta/run/stream")},
  {
    name: "messages.jsonl after two turns",
    provider: {deltas: ["part one ", "part two"], inputTokens: 42},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/beta/reset", {body: {}});
      await sse(c, "/api/sessions/beta/run", {method: "POST", body: {input: "first question"}});
      await sse(c, "/api/sessions/beta/run", {method: "POST", body: {input: "second question"}});
      const raw = await readFile(join(c.sessions, "beta", "messages.jsonl"), "utf8");
      const records = raw.split("\n").filter(Boolean).map((line) => JSON.parse(line));
      return {recordCount: records.length, records, providerInputs: c.provider.requests.map((r) => r.input)};
    },
  },
  {
    name: "history paging",
    run: async (c) => ({
      latest: await call(c, "GET", "/api/sessions/beta?turns=1"),
      history: await call(c, "GET", "/api/sessions/beta/history"),
      zero: await call(c, "GET", "/api/sessions/beta?turns=0"),
    }),
  },
  {
    name: "message fetch unknown",
    run: (c) => call(c, "GET", "/api/sessions/beta/messages/nope"),
  },
  {
    name: "attach after completion",
    provider: {text: "attach me"},
    run: async (c) => {
      await sse(c, "/api/sessions/alpha/run", {method: "POST", body: {input: "go"}});
      const attached = await sse(c, "/api/sessions/alpha/run/stream");
      return {status: attached.status, names: eventNames(attached)};
    },
  },
  {
    name: "run rejects bad profile",
    run: (c) => call(c, "POST", "/api/sessions/alpha/run?profile=nope", {body: {input: "x"}}),
  },
  {
    name: "run rejects oversized body",
    run: (c) => call(c, "POST", "/api/sessions/alpha/run", {body: {input: "x".repeat(1024 * 1024 + 10)}}),
  },
  {
    name: "tool run: native read across two rounds",
    provider: {steps: [toolRound("read", {path: "README.md"}), {text: "read it"}]},
    run: async (c) => {
      const result = await sse(c, "/api/sessions/tooled/run", {method: "POST", body: {input: "read the readme"}});
      return {
        result,
        records: await messagesJsonl(c, "tooled"),
        providerInputs: c.provider.requests.map((r) => r.input),
        providerToolNames: c.provider.requests.map((r) => (r.tools ?? []).map((t) => t.name)),
        snapshot: await call(c, "GET", "/api/sessions/tooled"),
      };
    },
  },
  {
    name: "tool run: invalid tool arguments are reported to the model",
    provider: {steps: [toolRound("read", {path: ""}), {text: "recovered"}]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/tooled/reset", {body: {}});
      const result = await sse(c, "/api/sessions/tooled/run", {method: "POST", body: {input: "bad args"}});
      return {names: eventNames(result), records: await messagesJsonl(c, "tooled")};
    },
  },
  {
    name: "tool run: unknown tool name",
    provider: {steps: [toolRound("nope", {}), {text: "ok"}]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/tooled/reset", {body: {}});
      const result = await sse(c, "/api/sessions/tooled/run", {method: "POST", body: {input: "unknown tool"}});
      return {names: eventNames(result), records: await messagesJsonl(c, "tooled")};
    },
  },
  {
    name: "request_draft: accept",
    provider: {steps: [toolRound("request_draft", {title: "Plan", draft: {goal: "ship", scope: ["a", "b"]}}), {text: "proceeding"}]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/tooled/reset", {body: {}});
      let replied;
      const result = await sseLive(c, "/api/sessions/tooled/run", {
        body: {input: "draft it", supports_interaction: true},
        onEvent: async (event) => {
          if (event.event === "interaction" && event.data.pending && !replied) {
            replied = await call(c, "POST", "/api/sessions/tooled/run/interaction", {body: {interaction_id: event.data.pending.id, decision: "accept"}});
          }
        },
      });
      const pending = result.events.find((e) => e.event === "interaction" && e.data.pending)?.data.pending;
      return {names: eventNames(result), pending, replied, records: await messagesJsonl(c, "tooled")};
    },
  },
  {
    name: "request_draft: update with edited draft",
    provider: {steps: [toolRound("request_draft", {title: "Plan", draft: {goal: "ship", scope: ["a", "b"]}}), {text: "using edits"}]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/tooled/reset", {body: {}});
      let replied;
      const result = await sseLive(c, "/api/sessions/tooled/run", {
        body: {input: "draft it", supports_interaction: true},
        onEvent: async (event) => {
          if (event.event === "interaction" && event.data.pending && !replied) {
            replied = await call(c, "POST", "/api/sessions/tooled/run/interaction", {body: {interaction_id: event.data.pending.id, decision: "update", draft: {goal: "ship fast", scope: ["a"], extra: true}}});
          }
        },
      });
      return {names: eventNames(result), replied, records: await messagesJsonl(c, "tooled")};
    },
  },
  {
    name: "request_draft: decline needs a reason, then declines",
    provider: {steps: [toolRound("request_draft", {draft: {goal: "ship"}}), {text: "stopping"}]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/tooled/reset", {body: {}});
      const attempts = [];
      const result = await sseLive(c, "/api/sessions/tooled/run", {
        body: {input: "draft it", supports_interaction: true},
        onEvent: async (event) => {
          if (event.event === "interaction" && event.data.pending && attempts.length === 0) {
            const id = event.data.pending.id;
            attempts.push(await call(c, "POST", "/api/sessions/tooled/run/interaction", {body: {interaction_id: id, decision: "decline"}}));
            attempts.push(await call(c, "POST", "/api/sessions/tooled/run/interaction", {body: {interaction_id: "stale", decision: "accept"}}));
            attempts.push(await call(c, "POST", "/api/sessions/tooled/run/interaction", {body: {interaction_id: id, decision: "maybe"}}));
            attempts.push(await call(c, "POST", "/api/sessions/tooled/run/interaction", {body: {interaction_id: id, decision: "decline", reason: "not now"}}));
          }
        },
      });
      return {names: eventNames(result), attempts, records: await messagesJsonl(c, "tooled")};
    },
  },
  {
    name: "request_draft: extend deadline",
    provider: {steps: [toolRound("request_draft", {draft: {goal: "ship"}}), {text: "extended"}]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/tooled/reset", {body: {}});
      const attempts = [];
      const result = await sseLive(c, "/api/sessions/tooled/run", {
        body: {input: "draft it", supports_interaction: true},
        onEvent: async (event) => {
          if (event.event === "interaction" && event.data.pending && attempts.length === 0) {
            const id = event.data.pending.id;
            attempts.push(await call(c, "POST", "/api/sessions/tooled/run/interaction/extend", {body: {interaction_id: id}}));
            attempts.push(await call(c, "POST", "/api/sessions/tooled/run/interaction/extend", {body: {interaction_id: "stale"}}));
            attempts.push(await call(c, "POST", "/api/sessions/tooled/run/interaction/extend", {body: {}}));
            attempts.push(await call(c, "POST", "/api/sessions/tooled/run/interaction", {body: {interaction_id: id, decision: "accept"}}));
          }
        },
      });
      return {names: eventNames(result), attempts};
    },
  },
  {
    name: "request_draft: headless run auto-accepts",
    provider: {steps: [toolRound("request_draft", {title: "Plan", draft: {goal: "ship"}}), {text: "went ahead"}]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/tooled/reset", {body: {}});
      const result = await sse(c, "/api/sessions/tooled/run", {method: "POST", body: {input: "draft it"}});
      return {names: eventNames(result), pendingEvents: result.events.filter((e) => e.event === "interaction"), records: await messagesJsonl(c, "tooled")};
    },
  },
  {
    name: "interaction endpoints without a run",
    run: async (c) => ({
      reply: await call(c, "POST", "/api/sessions/tooled/run/interaction", {body: {interaction_id: "x", decision: "accept"}}),
      extend: await call(c, "POST", "/api/sessions/tooled/run/interaction/extend", {body: {interaction_id: "x"}}),
    }),
  },
  {
    name: "compact a populated transcript",
    provider: {text: "summary of the conversation", inputTokens: 30},
    run: async (c) => {
      for (const input of ["first topic", "second topic", "third topic"]) {
        await sse(c, "/api/sessions/compactme/run", {method: "POST", body: {input}});
      }
      const before = await messagesJsonl(c, "compactme");
      const compact = await call(c, "POST", "/api/sessions/compactme/compact", {body: {}});
      const after = await messagesJsonl(c, "compactme");
      const snapshot = await call(c, "GET", "/api/sessions/compactme");
      const history = await call(c, "GET", "/api/sessions/compactme/history");
      const next = await sse(c, "/api/sessions/compactme/run", {method: "POST", body: {input: "after compaction"}});
      return {beforeCount: before.length, compact, after, snapshot, history, nextNames: eventNames(next), lastProviderInput: c.provider.requests.at(-1)?.input};
    },
  },
  {
    // compaction.keepbatch.tools is the number of pairs kept in total across the
    // kept turns, not per turn: three turns that each used one tool leave one pair.
    name: "compact with keepbatch.tools 1 over several turns",
    // One step list for all three turns: the fake provider numbers calls by its
    // request counter, so a reset between turns would reuse one call id.
    provider: {steps: ["a", "b", "c"].flatMap((word) => [toolRound("read", {path: "README.md"}), {text: "read " + word}])},
    run: async (c) => {
      for (const input of ["first topic", "second topic", "third topic"]) {
        await sse(c, "/api/sessions/keeptools/run", {method: "POST", body: {input}});
      }
      const compact = await call(c, "POST", "/api/sessions/keeptools/compact", {body: {}});
      const history = await call(c, "GET", "/api/sessions/keeptools/history");
      return {compact, history, records: await messagesJsonl(c, "keeptools")};
    },
  },
  {
    name: "compact twice",
    provider: {text: "second summary"},
    run: async (c) => ({
      again: await call(c, "POST", "/api/sessions/compactme/compact", {body: {}}),
      records: await messagesJsonl(c, "compactme"),
    }),
  },
  {
    name: "cursor paging across turns",
    provider: {text: "paged reply"},
    run: async (c) => {
      for (const input of ["one", "two", "three", "four"]) {
        await sse(c, "/api/sessions/paged/run", {method: "POST", body: {input}});
      }
      const first = await call(c, "GET", "/api/sessions/paged?turns=2");
      const cursor = first.body?.cursor ?? first.body?.next_cursor ?? first.body?.messages?.cursor;
      const pages = [first];
      let next = cursor;
      for (let i = 0; i < 4 && next !== undefined && next !== null; i += 1) {
        const page = await call(c, "GET", `/api/sessions/paged?turns=1&cursor=${encodeURIComponent(next)}`);
        pages.push(page);
        next = page.body?.cursor ?? page.body?.next_cursor;
      }
      return {pages, badCursor: await call(c, "GET", "/api/sessions/paged?cursor=garbage"), negative: await call(c, "GET", "/api/sessions/paged?turns=-1")};
    },
  },
  {
    name: "etag values and conditional requests",
    run: async (c) => {
      const headers = {authorization: `Bearer ${c.token}`};
      const read = async (path, extra = {}) => {
        const r = await fetch(`${c.base}${path}`, {headers: {...headers, ...extra}});
        return {status: r.status, etag: etagShape(r.headers.get("etag")), cacheControl: r.headers.get("cache-control"), length: (await r.text()).length > 0};
      };
      const snapshot = await fetch(`${c.base}/api/sessions/paged`, {headers});
      const tag = snapshot.headers.get("etag") ?? "";
      await snapshot.text();
      return {
        snapshot: await read("/api/sessions/paged"),
        matching: await read("/api/sessions/paged", {"if-none-match": tag}),
        weak: await read("/api/sessions/paged", {"if-none-match": `W/${tag}`}),
        list: await read("/api/sessions/paged", {"if-none-match": '"other"'}),
        history: await read("/api/sessions/paged/history"),
        sessions: await read("/api/sessions"),
      };
    },
  },
  {
    name: "native bash/write/edit run",
    provider: {steps: [
      toolRound("write", {path: "out/hello.txt", content: "one\ntwo\n"}),
      toolRound("edit", {path: "out/hello.txt", oldText: "two", newText: "three"}),
      toolRound("edit", {path: "out/hello.txt", oldText: "missing", newText: "x"}),
      toolRound("bash", {command: "cat out/hello.txt && echo done"}),
      toolRound("bash", {command: "echo oops >&2; exit 3"}),
      toolRound("write", {path: "out/b64.bin", content: "aGk=", contentEncoding: "base64"}),
      {text: "all done"},
    ]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/natived/reset", {body: {}});
      const result = await sse(c, "/api/sessions/natived/run", {method: "POST", body: {input: "do the work"}});
      const written = await readFile(join(c.root, "scratch", "out", "hello.txt"), "utf8");
      const binary = (await readFile(join(c.root, "scratch", "out", "b64.bin"))).toString("hex");
      return {names: eventNames(result), written, binary, records: await messagesJsonl(c, "natived"), snapshot: await call(c, "GET", "/api/sessions/natived")};
    },
  },
  {
    // The text piped to a command: plain, through a pipeline, under the `input`
    // alias, as multi-byte text, and ignored by the program. The natived session
    // allows 6 tool rounds, so each scenario stays under that.
    name: "bash stdin: piped text, pipeline, alias, multi-byte and ignored",
    provider: {steps: [
      toolRound("bash", {command: "cat", stdin: "piped-line\nsecond line\n"}),
      toolRound("bash", {command: "tr a-z A-Z | rev", stdin: "abc"}),
      toolRound("bash", {command: "cat", input: "via-input-alias"}),
      toolRound("bash", {command: "cat", stdin: "héllo ✓"}),
      toolRound("bash", {command: "echo ignored-input", stdin: "x".repeat(100 * 1024)}),
      {text: "stdin finished"},
    ]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/natived/reset", {body: {}});
      const result = await sse(c, "/api/sessions/natived/run", {method: "POST", body: {input: "pipe some text"}});
      return {names: eventNames(result), records: await messagesJsonl(c, "natived")};
    },
  },
  {
    // No stdin leaves the input closed, and a text over the default limit
    // (256 KiB) is refused whole: the command after the refusal must not run.
    name: "bash stdin: absent input is closed and an oversize text is refused",
    provider: {steps: [
      toolRound("bash", {command: "cat; echo no-input-done"}),
      toolRound("bash", {command: "echo should-not-run", stdin: "y".repeat(256 * 1024 + 1)}),
      {text: "stdin limit finished"},
    ]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/natived/reset", {body: {}});
      const result = await sse(c, "/api/sessions/natived/run", {method: "POST", body: {input: "pipe too much"}});
      return {names: eventNames(result), records: await messagesJsonl(c, "natived")};
    },
  },
  ...parallelScenarios(),
  ...approvalScenarios(),
  ...checkpointScenarios(),
  {
    name: "native tool argument errors",
    provider: {steps: [
      toolRound("write", {path: "", content: "x"}),
      toolRound("write", {path: "x.txt"}),
      toolRound("edit", {path: "out/hello.txt", oldText: "", newText: "x"}),
      toolRound("edit", {path: "nope/missing.txt", oldText: "a", newText: "b"}),
      toolRound("bash", {command: "sleep 5", timeout_ms: 50}),
      toolRound("bash", {command: ""}),
      toolRound("read", {path: "does-not-exist.txt"}),
      {text: "finished"},
    ]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/natived/reset", {body: {}});
      const result = await sse(c, "/api/sessions/natived/run", {method: "POST", body: {input: "break things"}});
      return {names: eventNames(result), records: await messagesJsonl(c, "natived")};
    },
  },
  {
    name: "edit with several occurrences",
    provider: {steps: [
      toolRound("edit", {path: "notes.txt", oldText: "alpha", newText: "omega"}),
      toolRound("edit", {path: "notes.txt", oldText: "alpha", newText: "omega", replaceAll: true}),
      toolRound("edit", {path: "notes.txt", oldText: "omega", newText: "omega", replaceAll: true}),
      {text: "edited"},
    ]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/natived/reset", {body: {}});
      const result = await sse(c, "/api/sessions/natived/run", {method: "POST", body: {input: "edit notes"}});
      return {names: eventNames(result), notes: await readFile(join(c.root, "scratch", "notes.txt"), "utf8"), records: await messagesJsonl(c, "natived")};
    },
  },
  {
    name: "run stop mid-stream",
    provider: {deltas: ["a", "b", "c", "d", "e", "f"], delayMs: 150},
    run: async (c) => {
      let stopped;
      const result = await sseLive(c, "/api/sessions/stopme/run", {
        body: {input: "stream slowly"},
        onEvent: async (event) => {
          if (event.event === "ui.delta" && stopped === undefined) stopped = await call(c, "POST", "/api/sessions/stopme/run/stop", {body: {}});
        },
      });
      const again = await call(c, "POST", "/api/sessions/stopme/run/stop", {body: {}});
      const idle = await sse(c, "/api/sessions/stopme/run/stream");
      const done = result.events.filter((e) => e.event === "done" || e.event === "error");
      return {stopped, again, idleNames: eventNames(idle), done, hasDelta: eventNames(result).includes("ui.delta"), records: await messagesJsonl(c, "stopme").catch(() => null), snapshot: await call(c, "GET", "/api/sessions/stopme")};
    },
  },
  {
    name: "run reattach mid-stream",
    provider: {deltas: ["one ", "two ", "three ", "four"], delayMs: 120},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/stopme/reset", {body: {}});
      let attached;
      const result = await sseLive(c, "/api/sessions/stopme/run", {
        body: {input: "stream and attach"},
        onEvent: async (event) => {
          if (event.event === "ui.delta" && attached === undefined) {
            attached = sse(c, "/api/sessions/stopme/run/stream");
            const concurrent = await call(c, "POST", "/api/sessions/stopme/run", {body: {input: "second run"}});
            attached = {pending: attached, concurrent};
          }
        },
      });
      const second = await attached.pending;
      const last = (events) => events.filter((e) => e.event === "message" || e.event === "ui.message" || e.event === "done").at(-1);
      return {first: eventNames(result), attachedNames: eventNames(second), attachedFirst: second.events[0], attachedLast: last(second.events), concurrent: attached.concurrent, firstLast: last(result.events)};
    },
  },
  {
    // A response larger than the live-run memory budget spills to a partial
    // file. A client attaching midway must still receive every byte so far, and
    // the saved transcript must hold the whole text.
    name: "run reattach mid-stream with a response larger than the memory budget",
    provider: {deltas: Array.from({length: 8}, (_, i) => `${String(i).repeat(30000)}\n`), delayMs: 120},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/stopme/reset", {body: {}});
      let attached;
      let partials;
      let count = 0;
      const result = await sseLive(c, "/api/sessions/stopme/run", {
        body: {input: "huge reply"},
        onEvent: async (event) => {
          if (event.event !== "ui.delta") return;
          count += 1;
          if (count === 5 && attached === undefined) {
            partials = await findFiles(join(c.root, "sessions"), /partial/);
            attached = sse(c, "/api/sessions/stopme/run/stream");
          }
        },
      });
      const second = await attached;
      const big = (value, found = []) => {
        if (typeof value === "string" && value.length > 1000) found.push({length: value.length, sha256: createHash("sha256").update(value).digest("hex")});
        else if (value && typeof value === "object") for (const child of Object.values(value)) big(child, found);
        return found;
      };
      const lastMessage = (events) => events.filter((e) => e.event === "message" || e.event === "ui.message").at(-1);
      const records = await messagesJsonl(c, "stopme");
      return {
        first: eventNames(result),
        attachedNames: [...new Set(eventNames(second))],
        attachedStartText: big(second.events.find((e) => e.event === "ui.message" || e.event === "message")),
        attachedEndText: big(lastMessage(second.events)),
        firstEndText: big(lastMessage(result.events)),
        spilledWhileRunning: partials,
        savedText: big(records),
        leftover: await findFiles(join(c.root, "sessions"), /partial/),
      };
    },
  },
  {
    name: "front compact: populated transcript",
    provider: {text: "front reply"},
    run: async (c) => {
      for (const input of ["alpha question", "beta question", "gamma question", "delta question"]) {
        await sse(c, "/api/sessions/frontc/run", {method: "POST", body: {input}});
      }
      const snapshot = await call(c, "GET", "/api/sessions/frontc");
      const ids = JSON.stringify(snapshot.body).match(/session-user-o\d+/g) ?? [];
      const unique = [...new Set(ids)];
      const target = unique[2];
      const before = await messagesJsonl(c, "frontc");
      const bad = [await call(c, "POST", "/api/sessions/frontc/front-compact", {body: {}}),
        await call(c, "POST", "/api/sessions/frontc/front-compact", {body: {message_id: "session-user-o01"}}),
        await call(c, "POST", "/api/sessions/frontc/front-compact", {body: {message_id: 5}}),
        await call(c, "POST", "/api/sessions/frontc/front-compact", {body: {message_id: "session-user-o999999"}})];
      const compact = await call(c, "POST", "/api/sessions/frontc/front-compact", {body: {message_id: target}});
      const after = await messagesJsonl(c, "frontc");
      const next = await call(c, "GET", "/api/sessions/frontc");
      const repeat = await call(c, "POST", "/api/sessions/frontc/front-compact", {body: {message_id: target}});
      const files = (await listSessionFiles(join(c.sessions, "frontc"))).filter((f) => f !== "messages.jsonl");
      return {ids: unique.length, target: target ?? null, beforeCount: before.length, bad, compact, afterCount: after.length, after, next, repeat, files};
    },
  },
  {
    name: "front compact: disabled and recoverable",
    provider: {text: "reply"},
    run: async (c) => {
      await sse(c, "/api/sessions/frontoff/run", {method: "POST", body: {input: "hello"}});
      const off = await call(c, "GET", "/api/sessions/frontoff");
      const offId = JSON.stringify(off.body).match(/session-user-o\d+/)?.[0];
      const disabled = await call(c, "POST", "/api/sessions/frontoff/front-compact", {body: {message_id: offId}});
      for (const input of ["one", "two", "three"]) await sse(c, "/api/sessions/frontrec/run", {method: "POST", body: {input}});
      const rec = await call(c, "GET", "/api/sessions/frontrec");
      const recIds = [...new Set(JSON.stringify(rec.body).match(/session-user-o\d+/g) ?? [])];
      const recoverable = await call(c, "POST", "/api/sessions/frontrec/front-compact", {body: {message_id: recIds[1]}});
      const files = (await listSessionFiles(join(c.sessions, "frontrec"))).map((f) => f.replace(/\d{4}-\d{2}-\d{2}/g, "$DATE"));
      return {disabled, recoverable, files};
    },
  },
  {
    name: "message fetch by id",
    provider: {text: "fetchable reply"},
    run: async (c) => {
      await sse(c, "/api/sessions/paged/run", {method: "POST", body: {input: "fetch me"}});
      const snapshot = await call(c, "GET", "/api/sessions/paged?turns=1");
      const ids = [...new Set(JSON.stringify(snapshot.body).match(/"id":\s*"([^"]+)"/g) ?? [])].slice(0, 6).map((s) => /"([^"]+)"$/.exec(s)[1]);
      const fetched = [];
      for (const id of ids) fetched.push({id, response: await call(c, "GET", `/api/sessions/paged/messages/${encodeURIComponent(id)}`)});
      const withCursor = ids[0] === undefined ? null : await call(c, "GET", `/api/sessions/paged/messages/${encodeURIComponent(ids[0])}?cursor=0`);
      const badCursor = await call(c, "GET", `/api/sessions/paged/messages/${encodeURIComponent(ids[0] ?? "x")}?cursor=nope`);
      return {ids, fetched, withCursor, badCursor};
    },
  },
  {
    name: "mcp: tool list and listing endpoint",
    provider: {text: "listed"},
    run: async (c) => {
      const tools = await call(c, "GET", "/api/sessions/mcp/tools");
      await call(c, "POST", "/api/sessions/mcp/reset", {body: {}});
      await sse(c, "/api/sessions/mcp/run", {method: "POST", body: {input: "what tools exist"}});
      return {tools, providerTools: c.provider.requests[0]?.tools?.map((t) => ({name: t.name, description: t.description, parameters: t.parameters, strict: t.strict, type: t.type}))};
    },
  },
  {
    name: "mcp: content types reach the model",
    provider: {steps: [
      toolRound(mcpName("fx", "echo"), {text: "hi"}),
      toolRound(mcpName("fx", "mixed"), {}),
      toolRound(mcpName("fx", "bad_media"), {}),
      toolRound(mcpName("fx", "empty"), {}),
      {text: "done"},
    ]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/mcp/reset", {body: {}});
      const result = await sse(c, "/api/sessions/mcp/run", {method: "POST", body: {input: "use mcp"}});
      return {names: eventNames(result), outputs: toolOutputs(c.provider.requests.at(-1)), records: await messagesJsonl(c, "mcp")};
    },
  },
  {
    name: "mcp: structured content keeps key order",
    provider: {steps: [toolRound(mcpName("fx", "structured"), {}), {text: "ok"}]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/mcp/reset", {body: {}});
      await sse(c, "/api/sessions/mcp/run", {method: "POST", body: {input: "structure"}});
      return {outputs: toolOutputs(c.provider.requests.at(-1)), records: await messagesJsonl(c, "mcp")};
    },
  },
  {
    name: "mcp: tool errors and protocol errors",
    provider: {steps: [
      toolRound(mcpName("fx", "tool_error"), {}),
      toolRound(mcpName("fx", "protocol_error"), {}),
      {text: "handled"},
    ]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/mcp/reset", {body: {}});
      const result = await sse(c, "/api/sessions/mcp/run", {method: "POST", body: {input: "fail"}});
      return {names: eventNames(result), outputs: toolOutputs(c.provider.requests.at(-1)), records: await messagesJsonl(c, "mcp")};
    },
  },
  {
    name: "mcp: invalid arguments are rejected locally",
    provider: {steps: [
      toolRound(mcpName("fx", "echo"), {}),
      toolRound(mcpName("fx", "echo"), {text: ""}),
      toolRound(mcpName("fx", "echo"), {text: "ok", extra: 1}),
      toolRound(mcpName("fx", "draft07"), {n: 0}),
      toolRound(mcpName("fx", "draft07"), {n: 3}),
      {text: "done"},
    ]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/mcp/reset", {body: {}});
      const result = await sse(c, "/api/sessions/mcp/run", {method: "POST", body: {input: "bad args"}});
      return {names: eventNames(result), outputs: toolOutputs(c.provider.requests.at(-1))};
    },
  },
  {
    name: "mcp: denied and unknown tools",
    provider: {steps: [
      toolRound(mcpName("fx", "denied"), {}),
      toolRound("mcp_fx_nothing_0000000000", {}),
      {text: "done"},
    ]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/mcp/reset", {body: {}});
      await sse(c, "/api/sessions/mcp/run", {method: "POST", body: {input: "denied"}});
      return {outputs: toolOutputs(c.provider.requests.at(-1))};
    },
  },
  {
    name: "mcp: oversized result is bounded",
    provider: {steps: [
      toolRound(mcpName("tiny", "big"), {size: 5000}),
      toolRound(mcpName("fx", "big"), {size: 3000}),
      toolRound(mcpName("fx", "big"), {size: 2000000}),
      {text: "done"},
    ]},
    run: async (c) => {
      await call(c, "POST", "/api/sessions/mcp/reset", {body: {}});
      await sse(c, "/api/sessions/mcp/run", {method: "POST", body: {input: "big"}});
      return {outputs: toolOutputs(c.provider.requests.at(-1)).map((o) => ({size: JSON.stringify(o).length, head: JSON.stringify(o).slice(0, 200), tail: JSON.stringify(o).slice(-200)}))};
    },
  },
  {
    name: "mcp: disabled server exposes nothing",
    provider: {text: "none"},
    run: async (c) => {
      const tools = await call(c, "GET", "/api/sessions/mcpdisabled/tools");
      await sse(c, "/api/sessions/mcpdisabled/run", {method: "POST", body: {input: "hello"}});
      return {tools, providerHadTools: c.provider.requests[0]?.tools ?? null};
    },
  },
  {
    name: "mcp: unreachable and misconfigured servers",
    provider: {text: "unused"},
    run: async (c) => {
      const out = {};
      for (const id of ["mcpmissing", "mcpbadcfg", "mcpnofile"]) {
        out[id] = {tools: await call(c, "GET", `/api/sessions/${id}/tools`), run: await call(c, "POST", `/api/sessions/${id}/run`, {body: {input: "hi"}})};
      }
      return {out, providerCalls: c.provider.requests.length};
    },
  },
  {
    name: "run failures before the provider is reached",
    provider: {text: "unused"},
    run: async (c) => {
      const out = {};
      for (const id of ["nokey", "badmodel", "toolmissing", "mcpmissing"]) {
        const run = await sse(c, `/api/sessions/${id}/run`, {method: "POST", body: {input: `hello ${id}`}});
        const records = await messagesJsonl(c, id).catch(() => null);
        out[id] = {run, records, snapshot: await call(c, "GET", `/api/sessions/${id}`), again: await sse(c, `/api/sessions/${id}/run`, {method: "POST", body: {input: "retry"}})};
      }
      return {out, providerCalls: c.provider.requests.length};
    },
  },
  {
    name: "maintenance with an unusable model.json",
    provider: {text: "unused"},
    run: async (c) => {
      const id = "badmodelmaint";
      for (const input of ["one", "two"]) await sse(c, `/api/sessions/${id}/run`, {method: "POST", body: {input}});
      // The snapshot is a 422 for this session, so take the second user record's
      // byte offset (which is the message id) from the transcript itself.
      const raw = await readFile(join(c.sessions, id, "messages.jsonl"), "utf8");
      const offsets = [];
      let at = 0;
      for (const line of raw.split("\n")) {
        if (line && JSON.parse(line).role === "user") offsets.push(at);
        at += Buffer.byteLength(line, "utf8") + 1;
      }
      const target = offsets[1] === undefined ? undefined : `session-user-o${offsets[1]}`;
      const before = await messagesJsonl(c, id);
      const compact = await call(c, "POST", `/api/sessions/${id}/compact`);
      const afterCompact = await messagesJsonl(c, id);
      const front = await call(c, "POST", `/api/sessions/${id}/front-compact`, {body: {message_id: target}});
      const afterFront = await messagesJsonl(c, id);
      const reset = await call(c, "POST", `/api/sessions/${id}/reset`);
      const afterReset = await messagesJsonl(c, id).catch(() => null);
      const final = await call(c, "GET", `/api/sessions/${id}`);
      return {target: target ?? null, beforeCount: before.length, compact, afterCompactCount: afterCompact.length, front, afterFrontCount: afterFront.length, reset, afterReset, final};
    },
  },
  ...providerFailureScenarios(),
  ...toolboxScenarios(),
  ...skillScenarios(),
  ...spillScenarios(),
  {name: "library root", run: (c) => call(c, "GET", "/api/library")},
  {name: "options preflight", run: async (c) => { const r = await fetch(`${c.base}/api/sessions`, {method: "OPTIONS", headers: {origin: "http://localhost:5173", "access-control-request-method": "POST"}}); return {status: r.status}; }},
];

// Results of one response's tool calls: the call ids of the tool_result parts in
// the order the stream published them, which is completion order when calls run
// in parallel.
const streamedResultOrder = (result) =>
  result.events.filter((e) => e.event === "ui.part" && e.data?.part?.type === "tool_result").map((e) => e.data.part.callId);

// Several calls in one provider response. The commands sleep for different
// times, so completion order differs from call order, and the transcript must
// still hold the results once each in call order.
function parallelScenarios() {
  const sleepCall = (callId, seconds) => ({name: "bash", callId, arguments: {command: `sleep ${seconds}; echo finished-${callId}`}});
  // `settle` names the calls that finish within microseconds of each other: their
  // relative order is scheduling noise (a JavaScript microtask queue and Go
  // goroutines legitimately differ), so only the set is compared for them.
  const run = (session, input, settle = []) => async (c) => {
    await call(c, "POST", `/api/sessions/${session}/reset`, {body: {}});
    const result = await sse(c, `/api/sessions/${session}/run`, {method: "POST", body: {input}});
    const records = await messagesJsonl(c, session);
    return {
      names: eventNames(result),
      streamedOrder: settle.length === 0 ? streamedResultOrder(result) : {settled: streamedResultOrder(result).filter((id) => settle.includes(id)).sort(), rest: streamedResultOrder(result).filter((id) => !settle.includes(id))},
      persistedOrder: records.flatMap((r) => (r.item?.type === "function_call_output" || r.type === "function_call_output" ? [r.item?.call_id ?? r.call_id] : [])),
      outputs: toolOutputs(c.provider.requests.at(-1)),
      records,
      snapshot: await call(c, "GET", `/api/sessions/${session}`),
    };
  };
  const calls = [sleepCall("c0", 1), sleepCall("c1", 0.2), sleepCall("c2", 0.5), sleepCall("c3", 0.1)];
  return [
    {name: "parallel tool calls stream in completion order with tool_call_scheduling concurrent", provider: {steps: [{calls}, {text: "all finished"}]}, run: run("parallelconc", "run four commands")},
    // The default, "ordered", runs a response that holds a call able to change state
    // one call at a time in the model's order, whatever each call's duration.
    {name: "default ordered scheduling runs bash calls in the model's order", provider: {steps: [{calls}, {text: "all finished"}]}, run: run("natived", "run four commands")},
    {
      // A response made only of read-only calls is not serialized: the slow read does
      // not hold up the others, so results still stream in completion order.
      name: "default ordered scheduling still runs a response of read-only calls together",
      provider: {steps: [{calls: [
        {name: "read", callId: "p0", arguments: {path: "notes.txt"}},
        {name: "read", callId: "p1", arguments: {path: "notes.txt", offset: 2}},
        {name: "read", callId: "p2", arguments: {path: "notes.txt", limit: 1}},
      ]}, {text: "read all"}]},
      run: run("natived", "read three things", ["p0", "p1", "p2"]),
    },
    {
      // Three chained edits and a read of one file in one response: every edit sees
      // the one before it and the read sees the last, as with a limit of one.
      name: "default ordered scheduling applies chained edits and a read in the model's order",
      provider: {steps: [{calls: [
        {name: "write", callId: "w0", arguments: {path: "chain.txt", content: "v0\n"}},
        {name: "edit", callId: "e1", arguments: {path: "chain.txt", oldText: "v0", newText: "v1"}},
        {name: "edit", callId: "e2", arguments: {path: "chain.txt", oldText: "v1", newText: "v2"}},
        {name: "edit", callId: "e3", arguments: {path: "chain.txt", oldText: "v2", newText: "v3"}},
        {name: "read", callId: "r1", arguments: {path: "chain.txt"}},
      ]}, {text: "chained"}]},
      run: async (c) => {
        await call(c, "POST", "/api/sessions/natived/reset", {body: {}});
        const result = await sse(c, "/api/sessions/natived/run", {method: "POST", body: {input: "chain edits"}});
        return {
          names: eventNames(result),
          streamedOrder: streamedResultOrder(result),
          outputs: toolOutputs(c.provider.requests.at(-1)),
          file: await readFile(join(c.root, "scratch", "chain.txt"), "utf8"),
        };
      },
    },
    {name: "max_parallel_tool_calls 1 runs calls in order", provider: {steps: [{calls: calls.slice(0, 3).map((entry, i) => sleepCall(`s${i}`, 0.1))}, {text: "all finished"}]}, run: run("parallelone", "run three commands")},
    {name: "max_parallel_tool_calls 2 bounds concurrency", provider: {steps: [{calls: [sleepCall("b0", 1), sleepCall("b1", 0.2), sleepCall("b2", 0.3), sleepCall("b3", 0.1)]}, {text: "all finished"}]}, run: run("paralleltwo", "run four commands")},
    {
      // A silent tool call keeps the run stream alive with comment frames. The
      // harness drops them elsewhere, so this scenario reads the raw text. The
      // exact count depends on timing, so only "several, and none of them events"
      // is compared.
      name: "quiet run stream gets keep-alive comment frames",
      provider: {steps: [{calls: [sleepCall("k0", 11.5)]}, {text: "slow done"}]},
      run: async (c) => {
        await call(c, "POST", "/api/sessions/natived/reset", {body: {}});
        const response = await fetch(`${c.base}/api/sessions/natived/run`, {method: "POST", headers: {authorization: `Bearer ${c.token}`, "content-type": "application/json"}, body: JSON.stringify({input: "one slow command"})});
        const raw = await response.text();
        const blocks = raw.split("\n\n").filter(Boolean);
        const keepAlives = blocks.filter((block) => block === ": keep-alive").length;
        return {status: response.status, severalKeepAlives: keepAlives >= 2, otherComments: blocks.filter((block) => block.startsWith(":") && block !== ": keep-alive"), completed: blocks.filter((block) => block.startsWith("event: completed")).length, done: blocks.filter((block) => block.startsWith("event: done")).length};
      },
    },
    {
      name: "parallel calls mix valid, invalid and unknown tools",
      provider: {steps: [{calls: [sleepCall("m0", 0.4), {name: "bash", callId: "m1", arguments: {command: ""}}, {name: "no_such_tool", callId: "m2", arguments: {}}, {name: "read", callId: "m3", arguments: {path: "notes.txt"}}]}, {text: "mixed"}]},
      run: run("parallelconc", "run a mix", ["m1", "m2", "m3"]),
    },
  ];
}

// Checkpoints: a run that changes files records the workspace before and after, and
// the user can list, dry-run and restore it. The scenarios compare what a client sees
// (record fields, plans, status codes, error text) and the files on disk, not how either
// backend takes the snapshot. Ids and times are masked by normalize.mjs.
function checkpointScenarios() {
  const space = (c, session) => join(c.root, `ckpt-${session}`);
  const put = async (c, session, name, text) => {
    const path = join(space(c, session), name);
    await mkdir(dirname(path), {recursive: true});
    await writeFile(path, text);
  };
  const file = async (c, session, name) => readFile(join(space(c, session), name), "utf8").catch(() => null);
  const list = async (c, session) => (await call(c, "GET", `/api/sessions/${session}/checkpoints`)).body;
  // The shape of a record without its identity: ids, trees and sizes differ per backend run.
  const shape = (record) => ({
    kind: record.kind, state: record.state, turn: record.turn_id?.replace(/^web-.*/, "web"), session_turn: record.session_turn,
    has_before: Boolean(record.before_tree), has_after: Boolean(record.after_tree), file_count: record.file_count ?? null,
    warning: record.warning ?? null, restored_from: Boolean(record.restored_from),
  });
  const resetAndRun = async (c, session, input) => {
    await call(c, "POST", `/api/sessions/${session}/reset`, {body: {}});
    return sse(c, `/api/sessions/${session}/run`, {method: "POST", body: {input}});
  };
  const restore = (c, session, id, body) => call(c, "POST", `/api/sessions/${session}/checkpoints/${id}/restore`, {body});
  const clear = async (c, session) => {
    await rm(space(c, session), {recursive: true, force: true});
    await mkdir(space(c, session), {recursive: true});
    await rm(join(c.sessions, session, "staging"), {recursive: true, force: true});
  };
  return [
    {
      name: "checkpoint: a run that writes records one checkpoint and a safe restore reverts it",
      provider: {steps: [{call: {name: "write", arguments: {path: "a.txt", content: "agent"}}}, {call: {name: "write", arguments: {path: "b.txt", content: "second"}}}, {text: "done"}]},
      run: async (c) => {
        await clear(c, "ckpt");
        await put(c, "ckpt", "a.txt", "original");
        await put(c, "ckpt", "keep.txt", "keep");
        const run = await resetAndRun(c, "ckpt", "write two files");
        const listed = await list(c, "ckpt");
        const [record] = listed.checkpoints;
        const dry = await restore(c, "ckpt", record.id, {dry_run: true});
        const filesAfterDry = [await file(c, "ckpt", "a.txt"), await file(c, "ckpt", "b.txt")];
        const done = await restore(c, "ckpt", record.id, {});
        const after = await list(c, "ckpt");
        return {
          names: eventNames(run), enabled: listed.enabled, records: listed.checkpoints.map(shape), dry, filesAfterDry,
          done: {...done, body: {restore: {...done.body.restore, undo_checkpoint_id: Boolean(done.body.restore?.undo_checkpoint_id), checkpoint_id: "id"}}},
          files: [await file(c, "ckpt", "a.txt"), await file(c, "ckpt", "b.txt"), await file(c, "ckpt", "keep.txt")],
          afterRecords: after.checkpoints.map(shape),
        };
      },
    },
    {
      name: "checkpoint: a read-only run records nothing and creates no files",
      provider: {steps: [{call: {name: "read", arguments: {path: "r.txt"}}}, {text: "done"}]},
      run: async (c) => {
        await clear(c, "ckpt");
        await put(c, "ckpt", "r.txt", "x");
        const run = await resetAndRun(c, "ckpt", "read it");
        return {names: eventNames(run), listed: await list(c, "ckpt"), repository: (await readdir(join(c.sessions, "ckpt", "staging")).catch(() => [])).filter((name) => name.startsWith("checkpoints"))};
      },
    },
    {
      name: "checkpoint: safe restore leaves a file changed after the run and reports it as a conflict",
      provider: {steps: [{call: {name: "write", arguments: {path: "a.txt", content: "agent"}}}, {call: {name: "write", arguments: {path: "d/b.txt", content: "agent-b"}}}, {text: "done"}]},
      run: async (c) => {
        await clear(c, "ckpt");
        await put(c, "ckpt", "a.txt", "a0");
        await put(c, "ckpt", "d/b.txt", "b0");
        await resetAndRun(c, "ckpt", "change both");
        const [record] = (await list(c, "ckpt")).checkpoints;
        await put(c, "ckpt", "a.txt", "user edit after the run");
        const done = await restore(c, "ckpt", record.id, {});
        return {status: done.status, plan: {...done.body.restore, undo_checkpoint_id: Boolean(done.body.restore?.undo_checkpoint_id), checkpoint_id: "id"}, files: [await file(c, "ckpt", "a.txt"), await file(c, "ckpt", "d/b.txt")]};
      },
    },
    {
      name: "checkpoint: a full restore makes the workspace equal to before the run, user edits included",
      provider: {steps: [{call: {name: "write", arguments: {path: "a.txt", content: "agent"}}}, {text: "done"}]},
      run: async (c) => {
        await clear(c, "ckpt");
        await put(c, "ckpt", "a.txt", "a0");
        await resetAndRun(c, "ckpt", "change it");
        const [record] = (await list(c, "ckpt")).checkpoints;
        await put(c, "ckpt", "a.txt", "user");
        await put(c, "ckpt", "later.txt", "later");
        const done = await restore(c, "ckpt", record.id, {mode: "full"});
        return {status: done.status, plan: {...done.body.restore, undo_checkpoint_id: Boolean(done.body.restore?.undo_checkpoint_id), checkpoint_id: "id"}, files: [await file(c, "ckpt", "a.txt"), await file(c, "ckpt", "later.txt")]};
      },
    },
    {
      name: "checkpoint: a restore is undone through the checkpoint it recorded",
      provider: {steps: [{call: {name: "write", arguments: {path: "a.txt", content: "agent"}}}, {text: "done"}]},
      run: async (c) => {
        await clear(c, "ckpt");
        await put(c, "ckpt", "a.txt", "a0");
        await resetAndRun(c, "ckpt", "change it");
        const [record] = (await list(c, "ckpt")).checkpoints;
        const first = await restore(c, "ckpt", record.id, {});
        const afterFirst = await file(c, "ckpt", "a.txt");
        const undoRecord = (await list(c, "ckpt")).checkpoints[0];
        const undone = await restore(c, "ckpt", undoRecord.id, {mode: "full"});
        return {firstStatus: first.status, afterFirst, undoShape: shape(undoRecord), undoneStatus: undone.status, afterUndo: await file(c, "ckpt", "a.txt")};
      },
    },
    {
      name: "checkpoint: a record left open by a stopped server is listed as interrupted and only a full restore works",
      provider: {steps: [{call: {name: "write", arguments: {path: "a.txt", content: "agent"}}}, {text: "done"}]},
      run: async (c) => {
        await clear(c, "ckpt");
        await put(c, "ckpt", "a.txt", "a0");
        await resetAndRun(c, "ckpt", "change it");
        // Leave the record the way a killed server does: still open, no run-end snapshot.
        const stored = join(c.sessions, "ckpt", "staging", "checkpoints.json");
        const document = JSON.parse(await readFile(stored, "utf8"));
        for (const record of document.checkpoints) {
          record.state = "open";
          delete record.after_tree;
          delete record.finished_at;
          delete record.warning;
        }
        await writeFile(stored, JSON.stringify(document));
        const listed = await list(c, "ckpt");
        const [record] = listed.checkpoints;
        const safe = await restore(c, "ckpt", record.id, {mode: "safe"});
        const full = await restore(c, "ckpt", record.id, {mode: "full"});
        return {
          records: listed.checkpoints.map(shape), safe: {status: safe.status, error: safe.body?.error?.replace(record.id, "id")},
          fullStatus: full.status, file: await file(c, "ckpt", "a.txt"),
        };
      },
    },
    {
      name: "checkpoint: a missing git binary is a failed record, the write still runs, and restore says 409",
      provider: {steps: [{call: {name: "write", arguments: {path: "a.txt", content: "hello"}}}, {text: "done"}]},
      run: async (c) => {
        await clear(c, "ckptnogit");
        const run = await resetAndRun(c, "ckptnogit", "write it");
        const listed = await list(c, "ckptnogit");
        const record = listed.checkpoints[0];
        const done = await restore(c, "ckptnogit", record.id, {});
        // Node and Go word the operating-system error differently; the prefix is the contract.
        return {names: eventNames(run), file: await file(c, "ckptnogit", "a.txt"), shape: {...shape(record), warning: record.warning?.split(":")[0] + ":" + record.warning?.split(":")[1]?.slice(0, 18)}, restore: done};
      },
    },
    {
      name: "checkpoint: restore routes answer 404 for an unknown id and 400 for a bad body, and list needs a token",
      run: async (c) => {
        const bad = [];
        for (const body of [{mode: "everything"}, {dry_run: "yes"}, {surprise: 1}, []]) bad.push(await restore(c, "ckpt", "ckpt-nope", body));
        return {
          unknown: await restore(c, "ckpt", "ckpt-nope", {}), bad,
          noToken: (await fetch(`${c.base}/api/sessions/ckpt/checkpoints`)).status,
          wrongMethod: await call(c, "POST", "/api/sessions/ckpt/checkpoints", {body: {}}),
          off: await list(c, "ckptoff"),
          offRestore: await restore(c, "ckptoff", "ckpt-nope", {}),
        };
      },
    },
    {
      // A restore is too quick to catch mid-way here, so this pins what both backends say
      // when none is running: the field is always sent, as a boolean, before and after.
      name: "checkpoint: the list always carries restoring, false when no restore is running",
      provider: {steps: [{call: {name: "write", arguments: {path: "a.txt", content: "agent"}}}, {text: "done"}]},
      run: async (c) => {
        await clear(c, "ckpt");
        await put(c, "ckpt", "a.txt", "original");
        const flag = async () => (await list(c, "ckpt")).restoring;
        const before = await flag();
        await resetAndRun(c, "ckpt", "write it");
        const afterRun = await flag();
        const [record] = (await list(c, "ckpt")).checkpoints;
        await restore(c, "ckpt", record.id, {dry_run: true});
        const afterDry = await flag();
        const done = await restore(c, "ckpt", record.id, {mode: "full"});
        return {before, afterRun, afterDry, afterRestore: await flag(), restoreStatus: done.status, file: await file(c, "ckpt", "a.txt")};
      },
    },
    {
      // git is slowed so the first restore stays open; a second one sent meanwhile must be refused
      // at once on both backends (Go used to queue it behind the first and run it afterwards).
      name: "checkpoint: a restore sent while another restore runs is refused with 409",
      provider: {steps: [{call: {name: "write", arguments: {path: "a.txt", content: "agent"}}}, {text: "done"}]},
      run: async (c) => {
        await clear(c, "ckptslow");
        await put(c, "ckptslow", "a.txt", "original");
        await resetAndRun(c, "ckptslow", "write it");
        const [record] = (await list(c, "ckptslow")).checkpoints;
        const flag = join(dirname(c.sessions), "slow-git.flag");
        await writeFile(flag, "slow");
        try {
          const first = restore(c, "ckptslow", record.id, {mode: "full"});
          let seen = false;
          for (let i = 0; i < 200 && !seen; i += 1) {
            seen = (await list(c, "ckptslow")).restoring === true;
            if (!seen) await new Promise((done) => setTimeout(done, 25));
          }
          const started = Date.now();
          const second = await restore(c, "ckptslow", record.id, {mode: "full"});
          const secondDry = await restore(c, "ckptslow", record.id, {dry_run: true});
          // Compaction, reset and front compaction share the restore's gate: refused at once too.
          const compact = await call(c, "POST", "/api/sessions/ckptslow/compact", {body: {}});
          const reset = await call(c, "POST", "/api/sessions/ckptslow/reset", {body: {}});
          const frontCompact = await call(c, "POST", "/api/sessions/ckptslow/front-compact", {body: {message_id: "session-user-o0"}});
          const refusedQuickly = Date.now() - started < 1000;
          const firstDone = await first;
          return {
            seen, second, secondDry, compact, reset, frontCompact, refusedQuickly, firstStatus: firstDone.status,
            afterRestoring: (await list(c, "ckptslow")).restoring, file: await file(c, "ckptslow", "a.txt"),
          };
        } finally {
          await rm(flag, {force: true});
        }
      },
    },
    {
      name: "checkpoint: a bad checkpoint value is refused with the same message",
      provider: {steps: [{text: "x"}]},
      run: async (c) => ({run: await call(c, "POST", "/api/sessions/ckptbad/run", {body: {input: "go"}}), list: await call(c, "GET", "/api/sessions/ckptbad/checkpoints")}),
    },
  ];
}

// Approval: a tool set to "ask" waits for the user before it runs. The scenarios
// answer through the HTTP endpoint, so they compare what a client sees (the
// published question and its kind, the refusal of an edit) and what the model
// sees (a blocked result with a code), not how either backend gets there.
function approvalScenarios() {
  const reply = (c, session, body) => call(c, "POST", `/api/sessions/${session}/run/interaction`, {body});
  // Runs one scripted response against `session`, calls `decide(c, pending, attempts)` when
  // the first question appears, and returns everything a client or the model observed.
  const asked = (name, session, steps, decide, {interaction = true, extra} = {}) => ({
    name,
    provider: {steps: [...steps, {text: "understood"}]},
    run: async (c) => {
      await call(c, "POST", `/api/sessions/${session}/reset`, {body: {}});
      const attempts = [];
      const questions = [];
      const result = await sseLive(c, `/api/sessions/${session}/run`, {
        body: {input: "go", supports_interaction: interaction},
        onEvent: async (event) => {
          if (event.event !== "interaction" || !event.data.pending) return;
          // A republished question (an extension) keeps its id; answer each question once.
          if (questions.some((q) => q.id === event.data.pending.id)) return;
          questions.push(event.data.pending);
          await decide(c, event.data.pending, attempts);
        },
      });
      return {
        names: eventNames(result),
        questions: questions.map((q) => ({kind: q.kind ?? null, tool: q.tool, tool_call_id: q.tool_call_id, title: q.title ?? null, draft: q.draft, extend_ms: q.extend_ms})),
        attempts,
        outputs: toolOutputs(c.provider.requests.at(-1)),
        records: await messagesJsonl(c, session),
        ...(extra ? await extra(c) : {}),
      };
    },
  });
  const accept = async (c, pending, attempts, session = "askall") => {
    attempts.push(await reply(c, session, {interaction_id: pending.id, decision: "accept"}));
  };
  const fileOf = (name) => async (c) => ({file: await readFile(join(c.root, "scratch", name), "utf8").catch(() => null)});
  const endSteps = [toolRound("write", {path: "ask/k.txt", content: "k"}), {text: "understood"}];
  return [
    asked("approval: accepted write runs and the question is published as an approval", "askall",
      [toolRound("write", {path: "ask/a.txt", content: "hello"})], accept, {extra: fileOf("ask/a.txt")}),
    asked("approval: declined write is blocked with the reason and writes nothing", "askall",
      [toolRound("write", {path: "ask/b.txt", content: "no"})],
      async (c, pending, attempts) => {
        attempts.push(await reply(c, "askall", {interaction_id: pending.id, decision: "decline"}));
        attempts.push(await reply(c, "askall", {interaction_id: pending.id, decision: "decline", reason: "use a copy"}));
      }, {extra: fileOf("ask/b.txt")}),
    asked("approval: update is refused for an approval and the question stays open", "askall",
      [toolRound("write", {path: "ask/c.txt", content: "x"})],
      async (c, pending, attempts) => {
        attempts.push(await reply(c, "askall", {interaction_id: pending.id, decision: "update", draft: {path: "/etc/passwd", content: "x"}}));
        await accept(c, pending, attempts);
      }, {extra: fileOf("ask/c.txt")}),
    asked("approval: bash and edit ask, read does not", "askall",
      [{calls: [
        {name: "read", callId: "r1", arguments: {path: "notes.txt"}},
        {name: "bash", callId: "b1", arguments: {command: "echo ran-b1"}},
      ]}], accept),
    asked("approval: edit asks and shows old and new text", "askall",
      [toolRound("edit", {path: "notes.txt", oldText: "beta", newText: "gamma"})], accept),
    asked("approval: long text is cut and says how long it was", "askall",
      [toolRound("bash", {command: `echo ${"y".repeat(200)}`, cwd: "."})], accept),
    // The question must show the text piped to the command, not just `python3 -`.
    asked("approval: a bash call shows its stdin, cut and counted when long", "askall",
      [toolRound("bash", {command: "cat", stdin: `print-me ${"z".repeat(5000)}`})], accept),
    // The bash call between them gives the started process time to exit, so the
    // list that follows sees the same state in both backends. It asks as well.
    asked("approval: process start asks, status does not", "askall",
      [toolRound("process", {action: "start", command: "echo started"}), toolRound("bash", {command: "sleep 0.6"}), toolRound("process", {action: "list"})], accept),
    asked("approval: headless run with deny blocks the call", "askall",
      [toolRound("write", {path: "ask/d.txt", content: "x"})], async () => {}, {interaction: false, extra: fileOf("ask/d.txt")}),
    asked("approval: headless run with allow runs the call", "askallow",
      [toolRound("write", {path: "ask/e.txt", content: "x"})], async () => {}, {interaction: false, extra: fileOf("ask/e.txt")}),
    {
      // Which of two simultaneous questions is asked first is scheduling (the TypeScript
      // backend and Go may differ, and the order of one response's calls is not promised),
      // so this compares what must hold either way: each call is asked once, never two
      // questions are open together, both run once answered, and the transcript keeps call order.
      name: "approval: two gated calls of one response are asked one at a time",
      provider: {steps: [{calls: [
        {name: "write", callId: "w1", arguments: {path: "ask/f1.txt", content: "1"}},
        {name: "write", callId: "w2", arguments: {path: "ask/f2.txt", content: "2"}},
      ]}, {text: "understood"}]},
      run: async (c) => {
        await call(c, "POST", "/api/sessions/askall/reset", {body: {}});
        const questions = [];
        let open = null;
        let overlapped = false;
        const result = await sseLive(c, "/api/sessions/askall/run", {
          body: {input: "go", supports_interaction: true},
          onEvent: async (event) => {
            if (event.event !== "interaction") return;
            const pending = event.data.pending;
            if (!pending) { open = null; return; }
            if (open !== null && open !== pending.id) overlapped = true;
            if (open === pending.id) return;
            open = pending.id;
            questions.push(pending);
            await reply(c, "askall", {interaction_id: pending.id, decision: "accept"});
          },
        });
        return {
          status: result.status,
          overlapped,
          asked: questions.map((q) => ({kind: q.kind ?? null, tool: q.tool, tool_call_id: q.tool_call_id, draft: q.draft})).sort((a, b) => (a.tool_call_id < b.tool_call_id ? -1 : 1)),
          completed: result.events.some((e) => e.event === "completed"),
          outputs: toolOutputs(c.provider.requests.at(-1)),
          persistedOrder: (await messagesJsonl(c, "askall")).flatMap((r) => (r.item?.type === "function_call_output" || r.type === "function_call_output" ? [r.item?.call_id ?? r.call_id] : [])),
          files: [await fileOf("ask/f1.txt")(c), await fileOf("ask/f2.txt")(c)],
        };
      },
    },
    // approval.default: no tool sets its own key, so the session default decides which ask.
    asked("approval default: a write with no approval key asks when the default is ask", "askdefault",
      [toolRound("write", {path: "ask/g.txt", content: "default"})],
      async (c, pending, attempts) => accept(c, pending, attempts, "askdefault"), {extra: fileOf("ask/g.txt")}),
    asked("approval default: bash asks, read does not", "askdefault",
      [{calls: [
        {name: "read", callId: "r1", arguments: {path: "notes.txt"}},
        {name: "bash", callId: "b1", arguments: {command: "echo default-b1"}},
      ]}], async (c, pending, attempts) => accept(c, pending, attempts, "askdefault")),
    asked("approval default: a process start asks and a list does not", "askdefault",
      [toolRound("process", {action: "start", command: "echo default-started"}), toolRound("bash", {command: "sleep 0.6"}), toolRound("process", {action: "list"})],
      async (c, pending, attempts) => accept(c, pending, attempts, "askdefault")),
    asked("approval default: declined write is blocked and writes nothing", "askdefault",
      [toolRound("write", {path: "ask/h.txt", content: "no"})],
      async (c, pending, attempts) => {
        attempts.push(await reply(c, "askdefault", {interaction_id: pending.id, decision: "decline", reason: "not by default"}));
      }, {extra: fileOf("ask/h.txt")}),
    asked("approval default: an explicit off on write wins and an explicit ask on edit asks", "askdefaultexplicit",
      [{calls: [
        {name: "write", callId: "w1", arguments: {path: "ask/i.txt", content: "one"}},
        {name: "edit", callId: "e1", arguments: {path: "ask/i.txt", oldText: "one", newText: "two"}},
      ]}], async (c, pending, attempts) => accept(c, pending, attempts, "askdefaultexplicit"), {extra: fileOf("ask/i.txt")}),
    asked("approval default: off asks nothing", "askdefaultoff",
      [toolRound("write", {path: "ask/j.txt", content: "quiet"}), toolRound("bash", {command: "echo quiet"})],
      async () => {}, {extra: fileOf("ask/j.txt")}),
    asked("approval default: an MCP server with no key asks when the default is ask", "askdefaultmcp",
      [toolRound(mcpName("fx", "echo"), {text: "hi"})], async (c, pending, attempts) => accept(c, pending, attempts, "askdefaultmcp")),
    {
      name: "approval default: null is refused with the same message",
      provider: {steps: [{text: "x"}]},
      run: async (c) => ({refused: await call(c, "POST", "/api/sessions/askdefaultbad/run", {body: {input: "go"}})}),
    },
    asked("approval: an MCP server set to ask holds its tool calls", "askmcp",
      [toolRound(mcpName("fx", "echo"), {text: "hi"})], async (c, pending, attempts) => accept(c, pending, attempts, "askmcp")),
    // The clearing event says why the question ended: who decided and what, a timeout or a
    // stop. The three runs go through the same question; only how it ends differs.
    {
      name: "approval: the event that clears a question says why it ended",
      provider: {steps: endSteps},
      run: async (c) => {
        const out = {};
        for (const [label, how] of [["allowed", "accept"], ["denied", "decline"], ["stopped", "stop"]]) {
          await call(c, "POST", "/api/sessions/askall/reset", {body: {}});
          c.provider.reset({steps: endSteps});
          let acted = false;
          const result = await sseLive(c, "/api/sessions/askall/run", {
            body: {input: "go", supports_interaction: true},
            onEvent: async (event) => {
              if (event.event !== "interaction" || !event.data.pending || acted) return;
              acted = true;
              if (how === "stop") await call(c, "POST", "/api/sessions/askall/run/stop", {body: {}});
              else await reply(c, "askall", {interaction_id: event.data.pending.id, decision: how, ...(how === "decline" ? {reason: "no"} : {})});
            },
          });
          out[label] = result.events.filter((e) => e.event === "interaction").map((e) => ({pending: e.data.pending ? "question" : null, ended: e.data.ended ?? null}));
        }
        return out;
      },
    },
    {
      name: "approval: a bad approval value is refused with the same message",
      provider: {steps: [{text: "x"}]},
      run: async (c) => {
        const out = {};
        for (const id of ["askbad", "askbadmcp", "askbadblock"]) {
          out[id] = await call(c, "POST", `/api/sessions/${id}/run`, {body: {input: "go"}});
        }
        return out;
      },
    },
  ];
}
