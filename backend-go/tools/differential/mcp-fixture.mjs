// Deterministic stdio MCP server used by the differential harness. Both the
// TypeScript and the Go backend spawn it, so any difference in what reaches the
// model comes from the client side. Tools exercise each content type the
// clients convert, plus error and oversized results.
import {dirname, join, resolve} from "node:path";
import {fileURLToPath, pathToFileURL} from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const sdk = (path) => import(pathToFileURL(join(repo, "backend/node_modules/@modelcontextprotocol/sdk/dist/esm", path)).href);
const {Server} = await sdk("server/index.js");
const {StdioServerTransport} = await sdk("server/stdio.js");
const {ListToolsRequestSchema, CallToolRequestSchema} = await sdk("types.js");

const object = (properties = {}, required = []) => ({type: "object", properties, required, additionalProperties: false});
const PNG = "iVBORw0KGgo=";
const PDF = Buffer.from("%PDF-1.4 fixture").toString("base64");

const tools = [
  {name: "echo", description: "Echo text back.", inputSchema: object({text: {type: "string", minLength: 1}}, ["text"])},
  {name: "mixed", description: "Every supported content type.", inputSchema: object()},
  {name: "bad_media", description: "Invalid image and resource data.", inputSchema: object()},
  {name: "structured", description: "Structured content with ordered keys.", inputSchema: object()},
  {name: "tool_error", description: "Reports a tool error.", inputSchema: object()},
  {name: "protocol_error", description: "Fails at the protocol level.", inputSchema: object()},
  {name: "big", description: "Oversized text.", inputSchema: object({size: {type: "integer", minimum: 1}})},
  {name: "empty", description: "Returns nothing.", inputSchema: object()},
  {name: "draft07", description: "Schema without a dialect marker.", inputSchema: {type: "object", properties: {n: {type: "integer", exclusiveMinimum: 0}}, required: ["n"]}},
  {name: "denied", description: "Hidden by deny_tools.", inputSchema: object()},
];

const handlers = {
  echo: ({text}) => ({content: [{type: "text", text: `echo: ${text}`}]}),
  mixed: () => ({content: [
    {type: "text", text: "plain text"},
    {type: "image", data: PNG, mimeType: "image/png"},
    {type: "resource", resource: {uri: "file:///tmp/notes.txt", text: "resource body", mimeType: "text/plain"}},
    {type: "resource", resource: {uri: "file:///tmp/report.pdf?v=1", blob: PDF, mimeType: "application/pdf"}},
    {type: "resource", resource: {uri: "file:///tmp/pic.png", blob: PNG, mimeType: "image/png"}},
    {type: "resource", resource: {uri: "file:///tmp/clip.wav", blob: PNG, mimeType: "audio/wav"}},
    {type: "resource_link", name: "Spec", uri: "https://example.com/spec", mimeType: "text/html"},
    {type: "audio", data: PNG, mimeType: "audio/mpeg"},
  ]}),
  bad_media: () => ({content: [
    {type: "image", data: "not base64!", mimeType: "image/png"},
    {type: "image", data: PNG, mimeType: "text/plain"},
    {type: "resource", resource: {uri: "file:///tmp/x.bin", blob: "***", mimeType: "application/octet-stream"}},
    {type: "resource", resource: {uri: "file:///tmp/y"}},
  ]}),
  structured: () => ({content: [{type: "text", text: "see structure"}], structuredContent: {zeta: 1, alpha: {"2": "b", "1": "a", b: [3, 2, 1]}, "10": true, "9": null, mid: "é\u2028"}}),
  tool_error: () => ({isError: true, content: [{type: "text", text: "something failed"}]}),
  protocol_error: () => { throw new Error("boom"); },
  big: ({size}) => ({content: [{type: "text", text: "x".repeat(size)}, {type: "text", text: "second block"}]}),
  empty: () => ({content: []}),
  draft07: ({n}) => ({content: [{type: "text", text: `n=${n}`}]}),
  denied: () => ({content: [{type: "text", text: "should never be reachable"}]}),
};

const server = new Server({name: "differential-fixture", version: "1.0.0"}, {capabilities: {tools: {}}});
server.setRequestHandler(ListToolsRequestSchema, async () => ({tools}));
server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const handler = handlers[request.params.name];
  if (!handler) throw new Error(`unknown tool ${request.params.name}`);
  return handler(request.params.arguments ?? {});
});
await server.connect(new StdioServerTransport());
