"use strict";
const path = require("path");
const dir = process.env.JAD_RUNNER_DIR;
const target = {};
globalThis.fn = new Proxy(target, {
  get(_, name) {
    if (typeof name !== "string" || name in target || name === "then" || name === "toJSON") return target[name];
    if (!/^[A-Za-z0-9][A-Za-z0-9_.-]*$/.test(name) || name.includes("..")) {
      throw new Error("fn." + name + " is not a helper name: use letters, digits, _ . - and start with a letter or digit; for a name with a dash write fn[\"my-helper\"]");
    }
    let file;
    try {
      if (!dir) throw new Error("no library");
      file = require.resolve(path.join(dir, name));
    } catch (error) {
      throw new Error("no helper fn." + name + " in " + (dir || "(the helper library does not exist)"));
    }
    return require(file);
  },
});
