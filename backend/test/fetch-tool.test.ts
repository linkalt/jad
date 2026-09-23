import assert from "node:assert/strict";
import {mkdir, mkdtemp, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {SessionConfigLoader, type NativeFetchToolSettings} from "../src/config.js";
import {executeNativeFetch} from "../src/tools/native/fetch.js";
import {FetchStore} from "../src/tools/native/fetch-store.js";

const policy: NativeFetchToolSettings = {
  storage_dir: ".fetch", default_limit: 12, max_limit: 24, max_response_bytes: 100_000,
  max_document_chars: 100_000, max_storage_bytes: 1_000_000, timeout_ms: 1_000,
  max_redirects: 3, allowed_hosts: [], denied_hosts: [],
};

async function runner(responses: Response[]) {
  const directory = await mkdtemp(join(tmpdir(), "agent-fetch-tool-"));
  const store = new FetchStore(join(directory, "fetch"));
  let sequence = 0;
  const fetchFn: typeof fetch = async () => {
    const response = responses.shift();
    if (!response) throw new Error("unexpected request");
    return response;
  };
  const run = (args: Record<string, unknown>) => executeNativeFetch(args, {callId: `call-${sequence += 1}`, workspaceRoot: directory, sessionDir: directory, policy, store, fetchFn});
  return {directory, run};
}

function cursor(result: string): string {
  const match = /^next_cursor: (.+)$/m.exec(result);
  assert.ok(match, "result should include a next cursor");
  return match[1]!;
}

test("normalizes fetch storage and policy settings", async () => {
  const directory = await mkdtemp(join(tmpdir(), "agent-fetch-config-"));
  try {
    const workspace = join(directory, "workspace");
    await mkdir(workspace);
    await writeFile(join(directory, "setting.json"), JSON.stringify({cwd: workspace, tools: {native: {fetch: {storage_dir: "state/fetch", allowed_hosts: ["Example.COM"], denied_hosts: ["blocked.example"]}}}}));
    const settings = await new SessionConfigLoader(directory).loadSettings();
    assert.equal(settings.tools.native.fetch?.storage_dir, join(workspace, "state/fetch"));
    assert.deepEqual(settings.tools.native.fetch?.allowed_hosts, ["example.com"]);
    assert.equal(settings.tools.native.fetch?.default_limit, 12_000);
    await writeFile(join(directory, "setting.json"), JSON.stringify({cwd: workspace, tools: {native: {fetch: {default_limit: 20, max_limit: 10}}}}));
    await assert.rejects(new SessionConfigLoader(directory).loadSettings(), /limits are incompatible/);
  } finally { await rm(directory, {recursive: true, force: true}); }
});

test("fetches HTML as stored paginated Markdown", async () => {
  const {directory, run} = await runner([
    new Response("<html><head><title>Example page</title></head><body><h1>Heading</h1><p>This is a longer body of text for pagination.</p></body></html>", {status: 200, headers: {"content-type": "text/html"}}),
  ]);
  try {
    const first = await run({url: "https://93.184.216.34/article", limit: 12});
    assert.match(first, /title: Example page/);
    let combined = first;
    let page = first;
    while (/^next_cursor: /m.test(page)) {
      page = await run({cursor: cursor(page), limit: 24});
      combined += page;
    }
    assert.match(combined, /# Headi/);
    assert.match(combined, /ng\n\nThis is a longer/);
    assert.doesNotMatch(combined, /fetch_failed/);
  } finally { await rm(directory, {recursive: true, force: true}); }
});

test("uses a URL when its supplied cursor is unavailable", async () => {
  const {directory, run} = await runner([
    new Response("<h1>Fallback</h1>", {status: 200, headers: {"content-type": "text/html"}}),
  ]);
  try {
    assert.match(await run({url: "https://93.184.216.34/fallback", cursor: "04ce2ea3-f4d3-46f4-bc46-1702d8a4e83e.2000"}), /Fallback/);
  } finally { await rm(directory, {recursive: true, force: true}); }
});

test("follows public redirects and rejects unsafe URLs without requesting them", async () => {
  const {directory, run} = await runner([
    new Response(null, {status: 302, headers: {location: "/next"}}),
    new Response("<h1>Next</h1>", {status: 200, headers: {"content-type": "text/html"}}),
  ]);
  try {
    assert.match(await run({url: "https://93.184.216.34/start"}), /final_url: https:\/\/93\.184\.216\.34\/next/);
    assert.match(await run({url: "http://127.0.0.1/private"}), /unsafe_url/);
    assert.match(await run({cursor: "bad"}), /fetch_storage_error/);
  } finally { await rm(directory, {recursive: true, force: true}); }
});
