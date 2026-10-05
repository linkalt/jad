// Generates config-parity.golden.json from the TypeScript SessionConfigLoader.
// Run: node testdata/gen-config-parity.mjs   (requires `npm run build` in backend/)
import {mkdtemp, mkdir, writeFile, rm} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join, dirname} from "node:path";
import {fileURLToPath, pathToFileURL} from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const {SessionConfigLoader} = await import(pathToFileURL(join(here, "../../../../backend/dist/src/config.js")).href);
const cases = JSON.parse(await (await import("node:fs/promises")).readFile(join(here, "config-parity.cases.json"), "utf8"));
const model = {schema_version: 1, provider: "openai", model: "gpt-test", api_key_env: "TEST_OPENAI_KEY", base_url: "https://api.example.test/v1/", request: {temperature: 0.2}};
const keys = ["messages", "messages_backup", "compaction_cache", "history_cache", "usage_cache", "cursor_cache", "run_error_cache", "statecontext", "staging", "http_debug", "tool_storage", "active_profile", "default_profile", "model_default", "models"];

const out = [];
for (const c of cases) {
  const root = await mkdtemp(join(tmpdir(), "cfgparity-"));
  const dir = join(root, "session-1");
  await mkdir(dir, {recursive: true});
  const setting = JSON.stringify(c.setting).replaceAll("$ROOT", root);
  await writeFile(join(dir, "setting.json"), setting);
  for (const name of ["model.json", "alternate.json", "other.json"]) await writeFile(join(dir, name), JSON.stringify(model));
  const sub = (s) => (typeof s === "string" ? s.replaceAll(root, "$ROOT") : s);
  const entry = {name: c.name, profile: c.profile ?? "", model: c.model ?? ""};
  try {
    const loader = new SessionConfigLoader(dir, {globalJadRoot: root, ...(c.profile ? {profile: c.profile} : {})});
    const loaded = await loader.load(c.model || undefined);
    const paths = {};
    for (const k of keys) if (loaded.settings.paths[k] !== undefined) paths[k] = JSON.parse(JSON.stringify(loaded.settings.paths[k], (_, v) => sub(v)));
    entry.ok = {paths, model_path: sub(loaded.paths.model_path)};
  } catch (error) {
    entry.error = sub(error.message).replaceAll(`${"$ROOT"}/session-1/setting.json: `, "");
  }
  out.push(entry);
  await rm(root, {recursive: true, force: true});
}
await writeFile(join(here, "config-parity.golden.json"), JSON.stringify(out, null, 2) + "\n");
console.log(`wrote ${out.length} cases`);
