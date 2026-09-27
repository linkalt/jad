import assert from "node:assert/strict";
import {mkdir, mkdtemp, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join, resolve} from "node:path";
import test from "node:test";

import {
  listRootLibrary,
  readRootLibraryAsset,
} from "../src/content-library.js";
import {loadRootLibraryDirectories} from "../src/session-roots.js";

async function writeSetting(
  settingPath: string,
  libraries: Record<string, unknown>,
): Promise<void> {
  await writeFile(
    settingPath,
    JSON.stringify({schema_version: 1, session_roots: [], libraries}),
  );
}

test("loads markdown libraries from relative and absolute global setting paths", async () => {
  const globalRoot = await mkdtemp(join(tmpdir(), "agent-library-"));
  const settingPath = join(globalRoot, "setting.json");
  const promptRoot = join(globalRoot, ".agent/prompts");
  const absoluteTemplates = join(globalRoot, "external/templates");
  const skillsRoot = join(globalRoot, ".agent/skills");
  try {
    await mkdir(join(promptRoot, "nested"), {recursive: true});
    await mkdir(absoluteTemplates, {recursive: true});
    await mkdir(join(skillsRoot, "summarize"), {recursive: true});
    await mkdir(join(skillsRoot, "bad-folder"), {recursive: true});
    await writeFile(
      join(promptRoot, "nested/review.md"),
      "---\nname: code-review\ndescription: Review a change.\n---\nReview this diff carefully.",
    );
    await writeFile(join(absoluteTemplates, "report.md"), "# Incident report\n\nFill in the details.");
    await writeFile(
      join(skillsRoot, "summarize/SKILL.md"),
      "---\nname: summarize\ndescription: Summarize supplied material.\n---\nUse concise bullets.",
    );
    await writeFile(
      join(skillsRoot, "bad-folder/SKILL.md"),
      "---\nname: wrong-name\ndescription: Invalid folder name.\n---\nDo not expose this skill.",
    );
    await writeSetting(settingPath, {
      prompt_dirs: [".agent/prompts"],
      template_dirs: [absoluteTemplates],
      skill_dirs: [".agent/skills"],
    });

    const directories = await loadRootLibraryDirectories({rootSettingPath: settingPath});
    assert.deepEqual(directories, {
      prompt_dirs: [resolve(promptRoot)],
      template_dirs: [resolve(absoluteTemplates)],
      skill_dirs: [resolve(skillsRoot)],
    });

    const catalog = await listRootLibrary({rootSettingPath: settingPath});
    assert.equal(catalog.prompts.length, 1);
    assert.equal(catalog.prompts[0]?.name, "code-review");
    assert.equal(catalog.prompts[0]?.source, "nested/review.md");
    assert.equal(catalog.templates.length, 1);
    assert.equal(catalog.templates[0]?.name, "Incident report");
    assert.equal(catalog.skills.length, 1);
    assert.equal(catalog.skills[0]?.name, "summarize");

    const promptId = catalog.prompts[0]!.id;
    await writeFile(
      join(promptRoot, "nested/review.md"),
      "---\nname: code-review\ndescription: Review a change.\n---\nUpdated review instructions.",
    );
    const freshPrompt = await readRootLibraryAsset("prompt", promptId, {rootSettingPath: settingPath});
    assert.equal(freshPrompt?.content, "Updated review instructions.");

    const replacementRoot = join(globalRoot, "alternate-prompts");
    await mkdir(replacementRoot, {recursive: true});
    await writeFile(join(replacementRoot, "new.md"), "New prompt text.");
    await writeSetting(settingPath, {
      prompt_dirs: [replacementRoot],
      template_dirs: [absoluteTemplates],
      skill_dirs: [".agent/skills"],
    });
    const updatedCatalog = await listRootLibrary({rootSettingPath: settingPath});
    assert.deepEqual(updatedCatalog.prompts.map((item) => item.name), ["new"]);
  } finally {
    await rm(globalRoot, {recursive: true, force: true});
  }
});

test("empty directory arrays disable a library and invalid arrays are rejected", async () => {
  const globalRoot = await mkdtemp(join(tmpdir(), "agent-library-config-"));
  const settingPath = join(globalRoot, "setting.json");
  try {
    await writeSetting(settingPath, {
      prompt_dirs: [],
      template_dirs: [],
      skill_dirs: [],
    });
    assert.deepEqual(await loadRootLibraryDirectories({rootSettingPath: settingPath}), {
      prompt_dirs: [],
      template_dirs: [],
      skill_dirs: [],
    });

    await writeSetting(settingPath, {prompt_dirs: ".agent/prompts"});
    await assert.rejects(
      () => loadRootLibraryDirectories({rootSettingPath: settingPath}),
      /libraries\.prompt_dirs must be an array/,
    );
  } finally {
    await rm(globalRoot, {recursive: true, force: true});
  }
});
