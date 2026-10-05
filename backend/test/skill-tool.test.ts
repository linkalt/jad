import assert from "node:assert/strict";
import {mkdir, mkdtemp, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {SessionConfigLoader} from "../src/config.js";
import {loadToolCatalog} from "../src/tools/index.js";
import {nativeSkillDefinition, normalizeSkillArguments} from "../src/tools/native/skill.js";
import {firstLine, informationFields, outputText} from "./tool-session.js";

function skillFile(name: string, description: string, body: string): string {
  return `---\nname: ${name}\ndescription: ${description}\n---\n${body}\n`;
}

async function addSkill(
  directory: string,
  name: string,
  description: string,
  body: string,
  folder: string = name,
): Promise<void> {
  await mkdir(join(directory, folder), {recursive: true});
  await writeFile(join(directory, folder, "SKILL.md"), skillFile(name, description, body), "utf8");
}

interface Fixture {
  root: string;
  sessionDir: string;
  first: string;
  second: string;
  run(args: Record<string, unknown>, callId?: string): Promise<string>;
  loadSettings(tool: Record<string, unknown> | undefined): ReturnType<SessionConfigLoader["loadSettings"]>;
}

async function withFixture(
  tool: Record<string, unknown> | undefined,
  body: (fixture: Fixture) => Promise<void>,
): Promise<void> {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-skill-"));
  try {
    const sessionDir = join(root, "session");
    const first = join(root, "skills-a");
    const second = join(root, "skills-b");
    await mkdir(sessionDir, {recursive: true});
    await mkdir(first, {recursive: true});
    await mkdir(second, {recursive: true});
    await writeFile(
      join(root, "setting.json"),
      JSON.stringify({libraries: {skill_dirs: ["skills-a", "skills-b"]}}),
    );
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(
      join(sessionDir, "tools.json"),
      JSON.stringify({schema_version: 1, tools: [nativeSkillDefinition]}),
    );
    const writeSessionSetting = async (settings: Record<string, unknown> | undefined) => {
      await writeFile(
        join(sessionDir, "setting.json"),
        JSON.stringify({
          cwd: sessionDir,
          tool_definition_paths: [join(sessionDir, "tools.json")],
          tools: {native: settings === undefined ? {} : {skill: settings}},
        }),
      );
    };
    await writeSessionSetting(tool);
    const loader = () => new SessionConfigLoader(sessionDir, {globalJadRoot: root});
    const catalog = await loadToolCatalog(await loader().load());
    await body({
      root,
      sessionDir,
      first,
      second,
      run: async (args, callId = "call_1") =>
        outputText(
          (
            await catalog.executeFunctionCall(
              {type: "function_call", call_id: callId, name: "skill", arguments: JSON.stringify(args)},
              {workspaceRoot: sessionDir, sessionDir},
            )
          ).output,
        ),
      loadSettings: async (settings) => {
        await writeSessionSetting(settings);
        return loader().loadSettings();
      },
    });
  } finally {
    await rm(root, {recursive: true, force: true});
  }
}

test("a skill tool without a name lists skills with their descriptions", async () => {
  await withFixture({}, async ({first, run}) => {
    await addSkill(first, "summarize", "Summarize a document.", "Body one.");
    await addSkill(first, "agent-tool-design", "Design tools for agents.", "Body two.");
    const output = await run({});
    assert.equal(firstLine(output).status, "ok");
    assert.equal(informationFields(output).skills, "2");
    assert.ok(output.includes("agent-tool-design: Design tools for agents.\nsummarize: Summarize a document."));
  });
});

test("an empty skill list is a successful result, not an error", async () => {
  await withFixture({}, async ({run}) => {
    const output = await run({});
    assert.equal(firstLine(output).status, "ok");
    assert.equal(informationFields(output).skills, "0");
  });
});

test("a skill tool with a name returns the instructions without frontmatter", async () => {
  await withFixture({}, async ({first, run}) => {
    await addSkill(first, "summarize", "Summarize a document.", "# Summarize\n\nKeep it short.");
    const output = await run({name: "summarize"});
    assert.equal(firstLine(output).status, "ok");
    const fields = informationFields(output);
    assert.equal(fields.name, "summarize");
    assert.equal(fields.directory, join(first, "summarize"));
    assert.ok(output.includes("# Summarize\n\nKeep it short."));
    assert.ok(!output.includes("description: Summarize a document."));
  });
});

test("harmless spelling variants of a skill name still load it", async () => {
  await withFixture({}, async ({first, run}) => {
    await addSkill(first, "agent-tool-design", "Design tools.", "Body.");
    for (const variant of ["Agent_Tool_Design", "  agent tool design  ", "AGENT-TOOL-DESIGN"]) {
      const output = await run({name: variant});
      assert.equal(firstLine(output).status, "ok", variant);
      assert.equal(informationFields(output).name, "agent-tool-design");
    }
    assert.deepEqual(normalizeSkillArguments({name: "   "}), {});
    assert.deepEqual(normalizeSkillArguments({}), {});
  });
});

test("an unknown skill name answers with the valid names so the model can correct it", async () => {
  await withFixture({}, async ({first, run}) => {
    await addSkill(first, "summarize", "Summarize.", "Body.");
    await addSkill(first, "review", "Review.", "Body.");
    const output = await run({name: "summarise"});
    assert.equal(firstLine(output).status, "not_found");
    const fields = informationFields(output);
    assert.equal(fields.error_code, "skill_not_found");
    assert.equal(fields.available, "review, summarize");
  });
});

test("the earlier configured directory wins when two define the same skill", async () => {
  await withFixture({}, async ({first, second, run}) => {
    await addSkill(first, "summarize", "From project.", "Project body.");
    await addSkill(second, "summarize", "From shared.", "Shared body.");
    await addSkill(second, "only-shared", "Only shared.", "Other body.");
    const loaded = await run({name: "summarize"});
    assert.ok(loaded.includes("Project body."));
    assert.ok(!loaded.includes("Shared body."));
    const listed = await run({});
    assert.equal(informationFields(listed).skills, "2");
    assert.ok(listed.includes("summarize: From project."));
    assert.ok(!listed.includes("From shared."));
  });
});

test("a skill whose folder does not match its name is not exposed", async () => {
  await withFixture({}, async ({first, run}) => {
    await addSkill(first, "real-name", "Mismatched.", "Hidden body.", "other-folder");
    assert.equal(informationFields(await run({})).skills, "0");
    assert.equal(firstLine(await run({name: "real-name"})).status, "not_found");
    assert.equal(firstLine(await run({name: "other-folder"})).status, "not_found");
  });
});

test("a skill name cannot reach files outside the skill directories", async () => {
  await withFixture({}, async ({root, first, run}) => {
    await addSkill(root, "outside", "Not in a skill directory.", "Secret body.");
    await addSkill(first, "inside", "Inside.", "Body.");
    for (const name of ["../outside", "..", "inside/../../outside", "/etc/passwd"]) {
      const output = await run({name});
      assert.equal(firstLine(output).status, "not_found", name);
      assert.ok(!output.includes("Secret body."), name);
    }
  });
});

test("a long skill is cut at max_bytes on a character boundary and says so", async () => {
  await withFixture({max_bytes: 11}, async ({first, run}) => {
    // Each "é" is two bytes, so a cut at 11 bytes would land inside a character.
    await addSkill(first, "long", "Long.", "ééééééééééééé");
    const output = await run({name: "long"});
    assert.equal(firstLine(output).status, "ok");
    const fields = informationFields(output);
    assert.equal(fields.truncated, "true");
    assert.equal(fields.content_bytes, "26");
    assert.ok(output.includes("ééééé\n"));
    assert.ok(!output.includes("ééééé" + "é"));
    assert.ok(!output.includes("\uFFFD"));
  });
});

test("a skill within max_bytes is returned whole without a truncation marker", async () => {
  await withFixture({max_bytes: 1000}, async ({first, run}) => {
    await addSkill(first, "short", "Short.", "Short body.");
    assert.equal(informationFields(await run({name: "short"})).truncated, undefined);
  });
});

test("skill directories are re-read on every call", async () => {
  await withFixture({}, async ({root, first, second, run}) => {
    await addSkill(first, "one", "First.", "Body.");
    await addSkill(second, "two", "Second.", "Body.");
    assert.equal(informationFields(await run({})).skills, "2");
    await writeFile(join(root, "setting.json"), JSON.stringify({libraries: {skill_dirs: ["skills-b"]}}));
    const output = await run({});
    assert.equal(informationFields(output).skills, "1");
    assert.equal(firstLine(await run({name: "one"})).status, "not_found");
  });
});

test("an unreadable global setting reports a recoverable error instead of throwing", async () => {
  await withFixture({}, async ({root, run}) => {
    await writeFile(join(root, "setting.json"), "{ not json");
    const output = await run({});
    assert.equal(firstLine(output).status, "error");
    assert.equal(informationFields(output).error_code, "skill_directories_unavailable");
  });
});

test("the skill tool schema takes one optional name and nothing else", async () => {
  await withFixture({}, async ({run}) => {
    assert.deepEqual(nativeSkillDefinition.inputSchema.required, undefined);
    const output = await run({name: "x", extra: true});
    assert.equal(firstLine(output).status, "invalid_input");
  });
});

test("skill settings default max_bytes and reject unknown or invalid values", async () => {
  await withFixture({}, async ({loadSettings}) => {
    assert.equal((await loadSettings({})).tools.native.skill?.max_bytes, 131_072);
    assert.equal((await loadSettings({max_bytes: 2048})).tools.native.skill?.max_bytes, 2048);
    assert.equal((await loadSettings(undefined)).tools.native.skill, undefined);
    await assert.rejects(loadSettings({max_byte: 10}), /tools\.native\.skill\.max_byte is not supported; did you mean max_bytes/);
    await assert.rejects(loadSettings({max_bytes: 0}), /tools\.native\.skill\.max_bytes must be a safe integer between 1 and/);
    await assert.rejects(loadSettings({max_bytes: 1.5}), /tools\.native\.skill\.max_bytes must be a safe integer/);
    await assert.rejects(loadSettings({max_bytes: 10 * 1024 * 1024 + 1}), /tools\.native\.skill\.max_bytes must be a safe integer/);
  });
});

test("the skill tool is absent when tools.native.skill is not configured", async () => {
  await withFixture(undefined, async ({run}) => {
    const output = await run({});
    assert.equal(informationFields(output).error_code, "unknown_tool");
  });
});
