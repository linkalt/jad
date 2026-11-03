// exec's fn for deno, loaded with --preload. As the node template, with require made for this
// module (deno has none) and helper files found by their extension (.ts, .js, ...).
import { createRequire } from "node:module";
import path from "node:path";
const require = createRequire(import.meta.url);
const dir = Deno.env.get("JAD_RUNNER_DIR");
const extensions = ["", ".ts", ".js", ".mts", ".mjs", ".cts", ".cjs"];
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
      file = resolveHelper(path.join(dir, name));
    } catch (error) {
      throw new Error("no helper fn." + name + " in " + (dir || "(the helper library does not exist)"));
    }
    return callable(require(file), name);
  },
});

// fn.<name> is also a call of the helper's entry: the default export, else the export named like the
// file (or with its - and . written _ or camelCase), else its only function. Every export stays a property.
function entryOf(module, name) {
  if (typeof module.default === "function") return module.default;
  const camel = name.replace(/[-._]+([A-Za-z0-9])/g, (_, c) => c.toUpperCase());
  for (const candidate of [name, name.replace(/[-.]/g, "_"), camel]) {
    if (typeof module[candidate] === "function") return module[candidate];
  }
  const functions = Object.keys(module).filter((key) => key !== "default" && typeof module[key] === "function");
  return functions.length === 1 ? module[functions[0]] : null;
}

function callable(module, name) {
  if (typeof module === "function" || module === null || typeof module !== "object") return module;
  return new Proxy(function () {}, {
    get: (_, key) => module[key],
    has: (_, key) => key in module,
    apply(_, self, args) {
      const entry = entryOf(module, name);
      if (!entry) {
        const exported = Object.keys(module).filter((key) => typeof module[key] === "function");
        throw new Error("fn." + name + " has no single entry to call (" + (exported.length ? "it exports " + exported.join(", ") + "; call fn." + name + "." + exported[0] + "(...)" : "it exports no function") + "): export a default function, or one named " + name);
      }
      return Reflect.apply(entry, module, args);
    },
  });
}

function resolveHelper(base) {
  for (const extension of extensions) {
    try {
      if (Deno.statSync(base + extension).isFile) return base + extension;
    } catch {
      // try the next extension
    }
  }
  throw new Error("not found");
}
