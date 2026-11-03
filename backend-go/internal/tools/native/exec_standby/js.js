// exec's standby shim: it stands in for the code of node -e, reads the call's code from fd 3 and
// runs it as a script named [eval]. Code that does not compile as a script (a syntax error, ES
// module syntax, top-level await) is declined with F on fd 4, so a fresh node -e runs it; R on
// fd 4 means the code runs here.
(() => {
  const fs = require("fs");
  let source;
  try { source = fs.readFileSync(3, "utf8"); } finally { fs.closeSync(3); }
  if (source === "") process.exit(0);
  let script;
  try {
    script = new (require("vm").Script)(source, { filename: "[eval]" });
  } catch {
    fs.writeSync(4, "F");
    process.exit(0);
  }
  fs.writeSync(4, "R");
  fs.closeSync(4);
  return script;
})().runInThisContext({ displayErrors: true });
