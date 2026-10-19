import { readFileSync, writeFileSync } from "node:fs";
import { createConnection } from "node:net";

export const input = JSON.parse(readFileSync(process.env.TOOL_SMITH_INPUT, "utf8"));

let socket;
let buffered = "";
let nextId = 1;
const pending = new Map();

function failAll(error) {
  for (const entry of pending.values()) entry.reject(error);
  pending.clear();
}

function onLine(line) {
  if (line.trim() === "") return;
  let frame;
  try {
    frame = JSON.parse(line);
  } catch {
    failAll(new Error("tool bridge: unreadable reply"));
    return;
  }
  const entry = pending.get(frame.id);
  if (frame.bridge_error !== undefined) {
    const error = new Error("tool bridge: " + frame.bridge_error);
    if (entry) {
      pending.delete(frame.id);
      entry.reject(error);
    } else {
      failAll(error);
    }
    return;
  }
  if (!entry) return;
  pending.delete(frame.id);
  const { id, ...result } = frame;
  entry.resolve(result);
}

function connect() {
  if (socket) return socket;
  socket = createConnection(process.env.TOOL_SMITH_SOCKET);
  socket.setEncoding("utf8");
  socket.on("data", (chunk) => {
    buffered += chunk;
    let end;
    while ((end = buffered.indexOf("\n")) >= 0) {
      const line = buffered.slice(0, end);
      buffered = buffered.slice(end + 1);
      onLine(line);
    }
  });
  socket.on("error", (error) => failAll(new Error("tool bridge: " + error.message)));
  socket.on("close", () => failAll(new Error("tool bridge: connection closed")));
  return socket;
}

function call(tool, args) {
  if (!process.env.TOOL_SMITH_SOCKET) {
    return Promise.resolve({
      ok: false,
      status: "invalid_input",
      summary: "tool " + tool + " not allowed; allowed: (none)",
      content: "",
      info: {},
    });
  }
  return new Promise((resolve, reject) => {
    const id = nextId++;
    pending.set(id, { resolve, reject });
    const frame = { id, token: process.env.TOOL_SMITH_TOKEN, tool, arguments: args === undefined ? {} : args };
    connect().write(JSON.stringify(frame) + "\n");
  });
}

export const tools = new Proxy(
  {},
  {
    get(_target, name) {
      if (typeof name !== "string" || name === "then") return undefined;
      return (args) => call(name, args);
    },
  },
);

export async function deliver(body) {
  try {
    const value = await body(input);
    if (value === undefined) return;
    const text = typeof value === "string" ? value : JSON.stringify(value);
    if (text !== undefined) writeFileSync(process.env.EXEC_RESULT, text);
  } catch (error) {
    console.error(error && error.stack ? error.stack : String(error));
    process.exitCode = 1;
  } finally {
    if (socket) socket.destroy();
  }
}
