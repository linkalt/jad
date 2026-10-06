// Offline differential harness: runs the TypeScript backend and the Go backend
// against identical fixtures and a shared fake Responses provider, replays the
// same HTTP requests, and compares normalized responses, SSE streams and
// messages.jsonl. No live provider is contacted.
//
//   cd backend && npm run build && cd ../backend-go && go build -o /tmp/agent-backend-go ./cmd/agent-backend
//   node backend-go/tools/differential/diff.mjs [--go-bin /tmp/agent-backend-go] [--update]
import {spawn} from "node:child_process";
import {createServer} from "node:http";
import {mkdtemp, mkdir, writeFile, readFile, rm} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join, dirname, resolve} from "node:path";
import {fileURLToPath} from "node:url";
import {scenarios} from "./scenarios.mjs";
import {normalize} from "./normalize.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const repo = resolve(here, "../../..");
const args = process.argv.slice(2);
const goBin = args.includes("--go-bin") ? args[args.indexOf("--go-bin") + 1] : "/tmp/agent-backend-go";
const TOKEN = "differential-token-0123456789abcdef0123456789";

const provider = await startFakeProvider();
const results = {ts: [], go: []};
let failures = 0;

for (const [label, start] of [["ts", startTs], ["go", startGo]]) {
  const root = await mkdtemp(join(tmpdir(), `diff-${label}-`));
  const sessions = join(root, "sessions");
  await mkdir(sessions, {recursive: true});
  await writeFixtures(sessions, provider.url);
  const server = await start(sessions);
  try {
    for (const scenario of scenarios) {
      provider.reset(scenario.provider ?? {});
      const context = {base: server.base, token: TOKEN, sessions, root, provider};
      let output;
      const started = Date.now();
      try {
        output = await scenario.run(context);
      } catch (error) {
        output = {error: String(error?.stack ?? error)};
      }
      if (Date.now() - started > 5000) console.log(`slow  ${label} ${scenario.name}: ${Date.now() - started} ms`);
      results[label].push({name: scenario.name, output: normalize(output, {root, sessions, port: server.port, providerUrl: provider.url})});
    }
  } finally {
    await server.stop();
    await rm(root, {recursive: true, force: true});
  }
}
await provider.close();

for (let index = 0; index < scenarios.length; index += 1) {
  const a = JSON.stringify(results.ts[index].output, null, 2);
  const b = JSON.stringify(results.go[index].output, null, 2);
  if (a === b) {
    console.log(`ok    ${scenarios[index].name}`);
    // --show <text> prints what the matching scenarios observed, to confirm a
    // scenario exercises what it claims to.
    const show = args.includes("--show") ? args[args.indexOf("--show") + 1] : undefined;
    if (show !== undefined && scenarios[index].name.includes(show)) console.log(a);
  } else {
    failures += 1;
    console.log(`DIFF  ${scenarios[index].name}`);
    await writeFile(`/tmp/diff-${index}-ts.json`, a);
    await writeFile(`/tmp/diff-${index}-go.json`, b);
    console.log(`      diff /tmp/diff-${index}-ts.json /tmp/diff-${index}-go.json`);
  }
}
console.log(failures === 0 ? `all ${scenarios.length} scenarios match` : `${failures} scenario(s) differ`);
process.exit(failures === 0 ? 0 : 1);

async function writeFixtures(sessions, providerUrl) {
  const model = {schema_version: 1, provider: "openai", model: "gpt-test", api_key_env: "DIFF_PROVIDER_KEY", base_url: `${providerUrl}/v1/`, request: {}};
  // The workspace is separate from the session store so the @file listing sees
  // only these fixture files, not transcripts that other scenarios create.
  const workspace = join(dirname(sessions), "workspace");
  await mkdir(join(workspace, "src", "nested"), {recursive: true});
  await mkdir(join(workspace, "node_modules", "pkg"), {recursive: true});
  for (const file of ["README.md", "src/main.ts", "src/nested/deep.ts", "node_modules/pkg/index.js", ".hidden"]) {
    await writeFile(join(workspace, file), `fixture ${file}\n`);
  }
  // Both servers load the same tool manifest, built from the TypeScript
  // definitions so a Go definition that drifted would show up as a difference.
  const definition = async (file, name) => (await import(join(repo, "backend/dist/src/tools/native", file)))[name];
  const nativeReadDefinition = await definition("read.js", "nativeReadDefinition");
  const nativeRequestDraftDefinition = await definition("request-draft.js", "nativeRequestDraftDefinition");
  const manifest = join(dirname(sessions), "tools.json");
  await writeFile(manifest, JSON.stringify({schema_version: 1, tools: [nativeReadDefinition, nativeRequestDraftDefinition]}));
  // Native tools that act on the filesystem run in their own directory, so the
  // files they create never show up in the @file listing of the other sessions.
  const scratch = join(dirname(sessions), "scratch");
  await mkdir(scratch, {recursive: true});
  await writeFile(join(scratch, "notes.txt"), "alpha\nbeta\nalpha\n");
  const mutatingManifest = join(dirname(sessions), "tools-mutating.json");
  await writeFile(mutatingManifest, JSON.stringify({
    schema_version: 1,
    tools: [
      nativeReadDefinition,
      await definition("bash.js", "nativeBashDefinition"),
      await definition("write.js", "nativeWriteDefinition"),
      await definition("edit.js", "nativeEditDefinition"),
    ],
  }));
  // The remaining native tools get their own workspace so their stores
  // (memory, tasks, processes) and the files grep and glob see are isolated.
  const toolbox = join(dirname(sessions), "toolbox");
  await mkdir(join(toolbox, "src", "lib"), {recursive: true});
  await mkdir(join(toolbox, "docs"), {recursive: true});
  await writeFile(join(toolbox, "src", "main.ts"), "import {helper} from './lib/helper';\nexport const answer = 42; // TODO: remove\nhelper(answer);\n");
  await writeFile(join(toolbox, "src", "lib", "helper.ts"), "export function helper(value: number) {\n  return value + 1; // TODO: check\n}\n");
  await writeFile(join(toolbox, "docs", "guide.md"), "# Guide\n\nThe answer is 42.\nTODO items live in src.\n");
  await writeFile(join(toolbox, "docs", "naïve café.txt"), "unicode name\n");
  await writeFile(join(toolbox, ".secret"), "hidden TODO\n");
  await writeFile(join(toolbox, "long.txt"), `${"x".repeat(5000)} needle ${"y".repeat(5000)}\n`);
  const askManifest = join(dirname(sessions), "tools-ask.json");
  await writeFile(askManifest, JSON.stringify({
    schema_version: 1,
    tools: [
      nativeReadDefinition,
      await definition("bash.js", "nativeBashDefinition"),
      await definition("write.js", "nativeWriteDefinition"),
      await definition("edit.js", "nativeEditDefinition"),
      await definition("process.js", "nativeProcessDefinition"),
    ],
  }));
  const toolboxManifest = join(dirname(sessions), "tools-toolbox.json");
  const toolboxTools = [];
  for (const [file, name] of [["read.js", "nativeReadDefinition"], ["grep.js", "nativeGrepDefinition"], ["glob.js", "nativeGlobDefinition"], ["memory.js", "nativeMemoryDefinition"], ["task.js", "nativeTaskDefinition"], ["process.js", "nativeProcessDefinition"], ["fetch.js", "nativeFetchDefinition"]]) {
    toolboxTools.push(await definition(file, name));
  }
  await writeFile(toolboxManifest, JSON.stringify({schema_version: 1, tools: toolboxTools}));
  // Skills come from the global root's setting.json. With JAD_SESSIONS_DIR set,
  // that root is the sessions directory itself. Two directories let a scenario
  // check that the earlier one wins a name clash.
  const skillManifest = join(dirname(sessions), "tools-skill.json");
  await writeFile(skillManifest, JSON.stringify({schema_version: 1, tools: [await definition("skill.js", "nativeSkillDefinition")]}));
  const skillsA = join(dirname(sessions), "skills-a");
  const skillsB = join(dirname(sessions), "skills-b");
  const addSkill = async (dir, name, description, body, folder = name) => {
    await mkdir(join(dir, folder), {recursive: true});
    await writeFile(join(dir, folder, "SKILL.md"), `---\nname: ${name}\ndescription: ${description}\n---\n${body}\n`);
  };
  await addSkill(skillsA, "summarize", "Summarize a document.", "# Summarize\n\nKeep it short.");
  await addSkill(skillsA, "agent-tool-design", "Design tools for agents.", "Body of the design skill.");
  await addSkill(skillsA, "mismatched", "Folder differs from its name.", "Hidden body.", "other-folder");
  await addSkill(skillsA, "accents", "Multi-byte body.", "é".repeat(200));
  await addSkill(skillsB, "summarize", "From the shared directory.", "Shared body.");
  await addSkill(skillsB, "only-shared", "Only in the later directory.", "Later body.");
  await writeFile(join(dirname(sessions), "outside-skill.md"), "secret\n");
  await writeFile(join(sessions, "setting.json"), JSON.stringify({libraries: {skill_dirs: [skillsA, skillsB]}}));
  // MCP servers: one stdio fixture process per server id, spawned by whichever
  // backend is under test. `tiny` caps results at the minimum to force truncation.
  const mcpDir = join(dirname(sessions), "mcp");
  await mkdir(mcpDir, {recursive: true});
  const fixture = join(here, "mcp-fixture.mjs");
  const stdio = {type: "stdio", command: "node", args: [fixture]};
  await writeFile(join(mcpDir, "fx.json"), JSON.stringify({schema_version: 1, transport: stdio}));
  await writeFile(join(mcpDir, "tiny.json"), JSON.stringify({schema_version: 1, transport: stdio, max_result_bytes: 512}));
  await writeFile(join(mcpDir, "missing-cmd.json"), JSON.stringify({schema_version: 1, transport: {type: "stdio", command: "/nonexistent/mcp-server"}}));
  await writeFile(join(mcpDir, "badtransport.json"), JSON.stringify({schema_version: 1, transport: {type: "carrier-pigeon"}}));
  const mcpServers = {
    mcp: [{id: "fx", config_path: join(mcpDir, "fx.json"), enabled: true, deny_tools: ["denied"]}, {id: "tiny", config_path: join(mcpDir, "tiny.json"), enabled: true, deny_tools: ["denied"]}],
    mcpdisabled: [{id: "fx", config_path: join(mcpDir, "fx.json"), enabled: false}],
    mcpmissing: [{id: "broken", config_path: join(mcpDir, "missing-cmd.json"), enabled: true}],
    mcpbadcfg: [{id: "bad", config_path: join(mcpDir, "badtransport.json"), enabled: true}],
    mcpnofile: [{id: "ghost", config_path: join(mcpDir, "absent.json"), enabled: true}],
  };
  for (const id of ["alpha", "beta", "profiled", "tooled", "compactme", "paged", "natived", "parallelone", "paralleltwo", "frontc", "frontoff", "frontrec", "stopme", "nokey", "badmodel", "badmodelmaint", "toolmissing", "provfail", "toolbox", "skilled", "skilledtiny", "skillbad", "procenv", "procenvbad", "spilldefault", "spillalways", "spilloff", "labelled", "askall", "askallow", "askmcp", "askbad", "askbadmcp", "askbadblock", "ckpt", "ckptnogit", "ckptbad", "ckptoff", ...Object.keys(mcpServers)]) {
    const dir = join(sessions, id);
    await mkdir(dir, {recursive: true});
    await writeFile(join(dir, "model.json"), JSON.stringify(model));
    await writeFile(join(dir, "alternate.json"), JSON.stringify(model));
    const setting = {schema_version: 1, cwd: workspace, session_name: id};
    if (id === "tooled") {
      setting.max_tool_rounds = 4;
      setting.tool_definition_paths = [manifest];
      setting.tools = {native: {read: {}, request_draft: {timeout_ms: 20000, max_timeout_ms: 40000, headless_auto_accept_timeout_ms: 300}}};
    }
    if (id === "natived" || id === "parallelone" || id === "paralleltwo") {
      setting.cwd = scratch;
      setting.max_tool_rounds = 6;
      setting.tool_definition_paths = [mutatingManifest];
      setting.tools = {native: {read: {}, bash: {}, write: {}, edit: {}}};
      // Bound how many calls of one response run at once; natived keeps the default.
      if (id === "parallelone") setting.max_parallel_tool_calls = 1;
      if (id === "paralleltwo") setting.max_parallel_tool_calls = 2;
    }
    // Bash output that exceeds the stream budget is spilled to files under the
    // session workspace; each session gets its own so the files can be listed.
    if (id.startsWith("spill")) {
      const workspaceDir = join(dirname(sessions), `${id}-workspace`);
      await mkdir(workspaceDir, {recursive: true});
      setting.cwd = workspaceDir;
      setting.max_tool_rounds = 8;
      setting.tool_definition_paths = [mutatingManifest];
      const spill = {spillalways: {keep: "always", cap_bytes: 100000}, spilloff: {enabled: false}}[id] ?? {};
      setting.tools = {native: {read: {}, bash: {spill}, write: {}, edit: {}}};
    }
    if (id === "toolbox") {
      setting.cwd = toolbox;
      setting.max_tool_rounds = 30;
      setting.tool_definition_paths = [toolboxManifest];
      setting.tools = {native: {read: {}, grep: {}, glob: {}, memory: {}, task: {}, process: {}, fetch: {}}};
    }
    if (id === "skilled" || id === "skilledtiny") {
      setting.cwd = toolbox;
      setting.max_tool_rounds = 20;
      setting.tool_definition_paths = [skillManifest];
      // skilledtiny cuts a skill, and the skill list, at 101 bytes: the list of
      // four skills is about 145 bytes, and the multi-byte skill is 400 bytes of
      // two-byte characters, so the cut falls inside a character and steps back.
      setting.tools = {native: {skill: id === "skilledtiny" ? {max_bytes: 101} : {}}};
    }
    if (id === "skillbad") {
      setting.cwd = toolbox;
      setting.tool_definition_paths = [skillManifest];
      setting.tools = {native: {skill: {max_byte: 10}}};
    }
    if (id === "procenv") {
      // process.env_file is loaded below the inline env, and env_deny removes last.
      setting.cwd = toolbox;
      setting.max_tool_rounds = 8;
      setting.tool_definition_paths = [toolboxManifest];
      setting.tools = {native: {process: {env_file: "process.env", env: {OVERRIDDEN: "inline"}, env_deny: ["DENIED"]}}};
      await writeFile(join(dir, "process.env"), ["# process-specific configuration", "FILE_VALUE=from-file", "OVERRIDDEN=from-file", 'QUOTED="line one\\nline two # literal"', "export DENIED=from-file", ""].join("\n"));
    }
    if (id === "procenvbad") {
      setting.cwd = toolbox;
      setting.tool_definition_paths = [toolboxManifest];
      setting.tools = {native: {process: {env_file: "missing.env"}}};
    }
    if (id === "nokey") await writeFile(join(dir, "model.json"), JSON.stringify({...model, api_key_env: "DIFF_UNSET_KEY"}));
    // Valid JSON that fails validation. A syntax error would only compare V8's
    // parser wording against Go's, which cannot match.
    if (id === "badmodel" || id === "badmodelmaint") await writeFile(join(dir, "model.json"), JSON.stringify({provider: "acme", api_key_env: "not a valid name"}));
    if (id === "toolmissing") setting.tool_definition_paths = [join(dirname(sessions), "no-such-tools.json")];
    // Approval: write, edit, bash and process ask; read does not. askallow lets an
    // unattended run proceed, askmcp gates a whole MCP server, and the ask*bad
    // sessions hold settings both backends must refuse with the same message.
    if (id === "askall" || id === "askallow" || id === "askbad" || id === "askbadblock") {
      setting.cwd = scratch;
      setting.max_tool_rounds = 6;
      setting.tool_definition_paths = [askManifest];
      setting.tools = {native: {read: {}, bash: {approval: "ask"}, write: {approval: "ask"}, edit: {approval: "ask"}, process: {approval: "ask"}}};
      setting.approval = {timeout_ms: 20000, max_timeout_ms: 40000, headless: id === "askallow" ? "allow" : "deny", preview_bytes: 64};
      if (id === "askbad") setting.tools.native.write.approval = "on";
      if (id === "askbadblock") setting.approval.headless = "maybe";
    }
    // Checkpoints: ckpt records runs, ckptnogit points at a git that does not exist,
    // ckptbad holds a value both backends must refuse with the same message, ckptoff
    // has the node absent. Each has its own workspace so a restore cannot touch another session.
    if (id === "ckpt" || id === "ckptnogit" || id === "ckptbad" || id === "ckptoff") {
      const ckptSpace = join(dirname(sessions), `ckpt-${id}`);
      await mkdir(ckptSpace, {recursive: true});
      setting.cwd = ckptSpace;
      setting.max_tool_rounds = 6;
      setting.tool_definition_paths = [askManifest];
      setting.tools = {native: {read: {paths: {allow: [ckptSpace]}}, write: {paths: {allow: [ckptSpace]}}, bash: {}, edit: {paths: {allow: [ckptSpace]}}}};
      if (id === "ckpt") setting.checkpoint = {enabled: true, max_checkpoints: 5};
      if (id === "ckptnogit") setting.checkpoint = {enabled: true, git_binary: "/nonexistent/git"};
      if (id === "ckptbad") setting.checkpoint = {enabled: true, snapshot_on: "always"};
    }
    if (id === "askmcp") {
      setting.max_tool_rounds = 6;
      setting.mcp_servers = [{id: "fx", config_path: join(mcpDir, "fx.json"), enabled: true, deny_tools: ["denied"], approval: "ask"}];
    }
    if (id === "askbadmcp") {
      setting.mcp_servers = [{id: "fx", config_path: join(mcpDir, "fx.json"), enabled: true, approval: null}];
    }
    if (mcpServers[id]) {
      setting.mcp_servers = mcpServers[id];
      setting.max_tool_rounds = 12;
    }
    if (id === "frontoff") setting.front_compaction = {enabled: false};
    if (id === "frontrec") setting.front_compaction = {delete_mode: "recoverable"};
    if (id === "compactme") {
      setting.compaction = {keepbatch: {turn: {type: "assistant_final", keep: 1}}};
    }
    if (id === "labelled") {
      // The active profile selects selected.json, whose provider and model differ
      // from model.json: the session list must name the selected file.
      await writeFile(join(dir, "selected.json"), JSON.stringify({...model, provider: "acme", model: "label-model"}));
      setting.paths = {active_profile: "one", profiles: {one: {model_default: "selected.json", models: ["selected.json", "model.json"]}}};
    }
    if (id === "profiled") {
      setting.paths = {active_profile: "one", profiles: {one: {models: ["model.json", "alternate.json"]}, two: {models: ["alternate.json"]}}};
    }
    await writeFile(join(dir, "setting.json"), JSON.stringify(setting));
  }
}

function serverEnv(sessions, port) {
  return {...process.env, JAD_SESSIONS_DIR: sessions, HOST: "127.0.0.1", PORT: String(port), AGENT_API_TOKEN: TOKEN, DIFF_PROVIDER_KEY: "sk-diff", JAD_ROOT_SETTING_PATH: ""};
}

async function startTs(sessions) {
  return launch("node", [join(repo, "backend/dist/src/server.js")], sessions, join(repo, "backend"));
}
async function startGo(sessions) {
  return launch(goBin, [], sessions, repo);
}

async function launch(command, commandArgs, sessions, cwd) {
  const port = await freePort();
  const child = spawn(command, commandArgs, {cwd, env: serverEnv(sessions, port), stdio: ["ignore", "pipe", "pipe"]});
  let log = "";
  child.stdout.on("data", (d) => (log += d));
  child.stderr.on("data", (d) => (log += d));
  const base = `http://127.0.0.1:${port}`;
  const deadline = Date.now() + 15000;
  for (;;) {
    try {
      if ((await fetch(`${base}/api/health`)).ok) break;
    } catch {}
    if (child.exitCode !== null || Date.now() > deadline) throw new Error(`server failed to start: ${command}\n${log}`);
    await new Promise((r) => setTimeout(r, 50));
  }
  return {
    base, port,
    async stop() {
      // --server-log prints what the server wrote, to diagnose a run that fails
      // with the generic "details were withheld" message.
      if (args.includes("--server-log")) console.log(`--- server log: ${command} ---\n${log}`);
      child.kill("SIGTERM");
      await new Promise((r) => { child.once("exit", r); setTimeout(() => { child.kill("SIGKILL"); r(); }, 3000); });
    },
  };
}

async function freePort() {
  return new Promise((res) => {
    const s = createServer();
    s.listen(0, "127.0.0.1", () => { const {port} = s.address(); s.close(() => res(port)); });
  });
}

// Fake OpenAI Responses endpoint. Each request pops the next scripted reply.
async function startFakeProvider() {
  let script = {};
  let counter = 0;
  const requests = [];
  const server = createServer(async (req, res) => {
    const chunks = [];
    for await (const c of req) chunks.push(c);
    const body = JSON.parse(Buffer.concat(chunks).toString() || "{}");
    requests.push(body);
    // A scenario may script rounds: each request consumes the next step, and the
    // last step repeats. A step is {text|deltas|inputTokens} or {call, reasoning}.
    const step = script.steps ? script.steps[Math.min(counter, script.steps.length - 1)] : script;
    const text = step.text ?? "hello from the fake provider";
    const deltas = step.deltas ?? [text];
    counter += 1;
    const id = `resp_fake_${counter}`;
    // A step may answer with an HTTP failure instead of a stream.
    if (step.status !== undefined) {
      res.writeHead(step.status, {"content-type": "application/json", ...(step.headers ?? {})});
      res.end(JSON.stringify(step.errorBody ?? {error: {message: "scripted failure SECRET-DETAIL", type: "scripted_error", code: null}}));
      return;
    }
    res.writeHead(200, {"content-type": "text/event-stream"});
    const send = (event) => res.write(`event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`);
    const response = {id, object: "response", status: "in_progress", model: body.model ?? "gpt-test", output: []};
    send({type: "response.created", sequence_number: 0, response});
    let seq = 1;
    // A step with `calls` answers with several function calls in one response,
    // like a model asking for parallel tool calls. Each entry is {name, arguments}.
    if (step.calls) {
      const items = step.calls.map((entry, index) => ({id: `fc_${counter}_${index}`, type: "function_call", status: "in_progress", call_id: entry.callId ?? `call_${counter}_${index}`, name: entry.name, arguments: ""}));
      const finished = [];
      for (const [index, item] of items.entries()) {
        const args = JSON.stringify(step.calls[index].arguments ?? {});
        send({type: "response.output_item.added", sequence_number: seq++, output_index: index, item});
        send({type: "response.function_call_arguments.delta", sequence_number: seq++, output_index: index, item_id: item.id, delta: args});
        send({type: "response.function_call_arguments.done", sequence_number: seq++, output_index: index, item_id: item.id, arguments: args});
        const done = {...item, status: "completed", arguments: args};
        finished.push(done);
        send({type: "response.output_item.done", sequence_number: seq++, output_index: index, item: done});
      }
      send({type: "response.completed", sequence_number: seq++, response: {...response, status: "completed", output: finished, usage: {input_tokens: step.inputTokens ?? 12, output_tokens: 7, total_tokens: 19, input_tokens_details: {cached_tokens: 0}, output_tokens_details: {reasoning_tokens: 0}}}});
      res.end();
      return;
    }
    if (step.call) {
      const call = {id: `fc_${counter}`, type: "function_call", status: "in_progress", call_id: step.call.callId ?? `call_${counter}`, name: step.call.name, arguments: ""};
      // Arguments may be a function of the requests so far, so a scenario can
      // reuse an id a previous tool call generated (task ids, memory refs).
      // A function may also stand in for a single field value.
      const resolve = (value) => (typeof value === "function" ? value(requests) : value);
      const rawArguments = resolve(step.call.arguments);
      const callArguments = rawArguments && typeof rawArguments === "object" ? Object.fromEntries(Object.entries(rawArguments).map(([k, v]) => [k, resolve(v)])) : rawArguments;
      const args = JSON.stringify(callArguments ?? {});
      send({type: "response.output_item.added", sequence_number: seq++, output_index: 0, item: call});
      send({type: "response.function_call_arguments.delta", sequence_number: seq++, output_index: 0, item_id: call.id, delta: args});
      send({type: "response.function_call_arguments.done", sequence_number: seq++, output_index: 0, item_id: call.id, arguments: args});
      const done = {...call, status: "completed", arguments: args};
      send({type: "response.output_item.done", sequence_number: seq++, output_index: 0, item: done});
      send({type: "response.completed", sequence_number: seq++, response: {...response, status: "completed", output: [done], usage: {input_tokens: step.inputTokens ?? 12, output_tokens: 7, total_tokens: 19, input_tokens_details: {cached_tokens: 0}, output_tokens_details: {reasoning_tokens: 0}}}});
      res.end();
      return;
    }
    const item = {id: `msg_${counter}`, type: "message", role: "assistant", status: "in_progress", content: []};
    send({type: "response.output_item.added", sequence_number: seq++, output_index: 0, item});
    send({type: "response.content_part.added", sequence_number: seq++, output_index: 0, item_id: item.id, content_index: 0, part: {type: "output_text", text: "", annotations: []}});
    let sent = 0;
    for (const delta of deltas) {
      if (step.delayMs) await new Promise((r) => setTimeout(r, step.delayMs));
      send({type: "response.output_text.delta", sequence_number: seq++, output_index: 0, item_id: item.id, content_index: 0, delta});
      sent += 1;
      // Drop the connection mid-stream, before response.completed.
      if (step.disconnectAfter !== undefined && sent >= step.disconnectAfter) {
        await new Promise((r) => setTimeout(r, 30));
        res.destroy();
        return;
      }
    }
    // End the stream cleanly but without response.completed.
    if (step.endWithoutCompleted) { res.end(); return; }
    const full = deltas.join("");
    const done = {...item, status: "completed", content: [{type: "output_text", text: full, annotations: []}]};
    send({type: "response.output_text.done", sequence_number: seq++, output_index: 0, item_id: item.id, content_index: 0, text: full});
    send({type: "response.output_item.done", sequence_number: seq++, output_index: 0, item: done});
    send({type: "response.completed", sequence_number: seq++, response: {...response, status: "completed", output: [done], usage: {input_tokens: step.inputTokens ?? 12, output_tokens: 7, total_tokens: 19, input_tokens_details: {cached_tokens: 0}, output_tokens_details: {reasoning_tokens: 0}}}});
    res.end();
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const url = `http://127.0.0.1:${server.address().port}`;
  return {
    url, requests,
    reset(next) { script = next; counter = 0; requests.length = 0; },
    close: () => new Promise((r) => server.close(r)),
  };
}
