// tool_smith's entry. The prelude loads first; the call's code (wrapper.ts) is imported once fd 3
// ends. A fresh start gets an ended fd 3 (/dev/null) and goes on at once; an interpreter started
// before the call (standby) waits here with the prelude loaded until the call has written its
// files. Both runs then take the same path. /dev/fd/3 is read by path, which bun, node and deno
// all allow (deno does not read an inherited descriptor by number).
import "./prelude.ts";
import { closeSync, readFileSync } from "node:fs";

readFileSync("/dev/fd/3");
try {
  closeSync(3);
} catch {
  // deno: the descriptor is not one of its own; it stays open, and ended.
}
await import(new URL("./wrapper.ts", import.meta.url).href);
