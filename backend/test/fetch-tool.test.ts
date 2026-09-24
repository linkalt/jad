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
  max_redirects: 3, include_metadata: false, include_images: false, allowed_hosts: [], denied_hosts: [],
};

async function runner(responses: Response[], settings: Partial<NativeFetchToolSettings> = {}) {
  const directory = await mkdtemp(join(tmpdir(), "agent-fetch-tool-"));
  const store = new FetchStore(join(directory, "fetch"));
  let sequence = 0;
  const fetchFn: typeof fetch = async () => {
    const response = responses.shift();
    if (!response) throw new Error("unexpected request");
    return response;
  };
  const run = (args: Record<string, unknown>) => executeNativeFetch(args, {callId: `call-${sequence += 1}`, workspaceRoot: directory, sessionDir: directory, policy: {...policy, ...settings}, store, fetchFn});
  return {directory, run};
}

function cursor(result: string): string {
  const match = /^next_cursor: (.+)$/m.exec(result);
  assert.ok(match, "result should include a next cursor");
  return match[1]!;
}

function pageContent(result: string): string {
  const match = /^--- CONTENT\b[^\n]*\n([\s\S]*?)\n--- END CONTENT/m.exec(result);
  assert.ok(match, "result should include a content block");
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
    assert.equal(settings.tools.native.fetch?.include_metadata, false);
    assert.equal(settings.tools.native.fetch?.include_images, false);
    await writeFile(join(directory, "setting.json"), JSON.stringify({cwd: workspace, tools: {native: {fetch: {include_metadata: true, include_images: true}}}}));
    const overrides = await new SessionConfigLoader(directory).loadSettings();
    assert.equal(overrides.tools.native.fetch?.include_metadata, true);
    assert.equal(overrides.tools.native.fetch?.include_images, true);
    await writeFile(join(directory, "setting.json"), JSON.stringify({cwd: workspace, tools: {native: {fetch: {include_images: "yes"}}}}));
    await assert.rejects(new SessionConfigLoader(directory).loadSettings(), /include_images must be a boolean/);
    await writeFile(join(directory, "setting.json"), JSON.stringify({cwd: workspace, tools: {native: {fetch: {default_limit: 20, max_limit: 10}}}}));
    await assert.rejects(new SessionConfigLoader(directory).loadSettings(), /limits are incompatible/);
  } finally { await rm(directory, {recursive: true, force: true}); }
});

test("fetches HTML as stored paginated Markdown", async () => {
  const {directory, run} = await runner([
    new Response("<html><head><title>Example page</title><meta name=description content='A description not needed in default output'><meta property=og:image content='/social.png'></head><body><h1>Heading</h1><img src='/hero.jpg' alt='Hero image'><p>This is a longer body of text for pagination.</body></html>", {status: 200, headers: {"content-type": "text/html"}}),
  ]);
  try {
    const first = await run({url: "https://93.184.216.34/article", limit: 12});
    assert.match(first, /title: Example page/);
    let markdown = pageContent(first);
    let page = first;
    while (/^next_cursor: /m.test(page)) {
      page = await run({cursor: cursor(page), limit: 24});
      markdown += pageContent(page);
    }
    assert.match(markdown, /# Heading/);
    assert.match(markdown, /This is a longer body of text for pagination/);
    assert.doesNotMatch(first, /fetch_failed/);
    assert.doesNotMatch(markdown, /meta-description:|meta-og:image:|!\[Hero image\]/);
  } finally { await rm(directory, {recursive: true, force: true}); }
});

test("fetches Markdown and other text MIME types without HTML conversion", async () => {
  const markdown = "# Readme -- **Markdown stays unchanged.**";
  const plainText = "<h1>Literal text</h1> Not HTML.";
  const json = JSON.stringify({title: "Example"});
  const {directory, run} = await runner([
    new Response(markdown, {status: 200, headers: {"content-type": "text/markdown; charset=utf-8"}}),
    new Response(plainText, {status: 200, headers: {"content-type": "text/plain"}}),
    new Response(json, {status: 200, headers: {"content-type": "application/problem+json; charset=utf-8"}}),
  ], {default_limit: 100, max_limit: 100});
  try {
    const markdownResult = await run({url: "https://93.184.216.34/README.md"});
    assert.equal(pageContent(markdownResult), markdown);
    const plainTextResult = await run({url: "https://93.184.216.34/plain.txt"});
    assert.equal(pageContent(plainTextResult), plainText);
    const jsonResult = await run({url: "https://93.184.216.34/data"});
    assert.equal(pageContent(jsonResult), json);
  } finally { await rm(directory, {recursive: true, force: true}); }
});

test("rejects binary MIME types even when the URL has a text-file extension", async () => {
  const response = new Response("not decoded as text", {status: 200, headers: {"content-type": "application/octet-stream"}});
  const {directory, run} = await runner([response]);
  try {
    const result = await run({url: "https://93.184.216.34/README.md"});
    assert.match(result, /unsupported_content_type/);
    assert.equal(response.bodyUsed, false);
  } finally { await rm(directory, {recursive: true, force: true}); }
});

test("allows metadata and Markdown images when enabled by fetch settings", async () => {
  const {directory, run} = await runner([
    new Response("<html><head><title>Configurable</title><meta name=description content='SEO description'></head><body><h1>Topic</h1><img src='/chart.png' alt='Chart'></body></html>", {status: 200, headers: {"content-type": "text/html"}}),
  ], {include_metadata: true, include_images: true, max_limit: 1_000});
  try {
    const result = await run({url: "https://93.184.216.34/configurable", limit: 1_000});
    assert.match(result, /meta-description: SEO description/);
    assert.match(result, /!\[Chart\]\(\/chart\.png\)/);
  } finally { await rm(directory, {recursive: true, force: true}); }
});

test("omits linked image content but preserves the destination link", async () => {
  const {directory, run} = await runner([
    new Response("<a href='/full-size.jpg'><img src='/preview.jpg' alt='Preview image'></a>", {status: 200, headers: {"content-type": "text/html"}}),
  ], {max_limit: 1_000});
  try {
    const result = await run({url: "https://93.184.216.34/linked-image", limit: 1_000});
    assert.match(result, /full-size\.jpg/);
    assert.doesNotMatch(result, /preview\.jpg|!\[/);
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
