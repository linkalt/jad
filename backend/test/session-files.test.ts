import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import type { NativePathPolicy, SessionFileListingSettings } from "../src/config.js";
import { createAgentServer } from "../src/server.js";
import { collectSessionFiles } from "../src/session-files.js";

const LISTING: SessionFileListingSettings = {
  enabled: true,
  max_depth: 12,
  max_entries: 5000,
  max_results: 2000,
  include_hidden: false,
  ignore_directories: ["node_modules", "dist"],
};

const OPEN_POLICY: NativePathPolicy = { paths: [], unrestricted_paths: false };

/** A tree with one of every kind of entry the walk has to decide about. */
async function writeTree(root: string): Promise<void> {
  await mkdir(join(root, "src/components"), { recursive: true });
  await mkdir(join(root, "node_modules/pkg"), { recursive: true });
  await mkdir(join(root, ".hidden"), { recursive: true });
  await writeFile(join(root, "README.md"), "# readme\n");
  await writeFile(join(root, ".env"), "SECRET=1\n");
  await writeFile(join(root, "." + "hidden/secret.txt"), "secret\n");
  await writeFile(join(root, "src/index.ts"), "export {};\n");
  await writeFile(join(root, "src/components/Button.tsx"), "export {};\n");
  await writeFile(join(root, "node_modules/pkg/index.js"), "module.exports = {};\n");
  await symlink(join(root, "README.md"), join(root, "link.md")).catch(() => undefined);
}

async function withTree(run: (root: string) => Promise<void>): Promise<void> {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-files-"));
  try {
    await writeTree(root);
    await run(root);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}

test("lists cwd files shallow-first, skipping hidden entries, ignored directories and symlinks", async () => {
  await withTree(async (root) => {
    const listing = await collectSessionFiles(root, LISTING, OPEN_POLICY);
    assert.deepEqual(listing.files, [
      "README.md",
      "src/index.ts",
      "src/components/Button.tsx",
    ]);
    assert.equal(listing.truncated, false);
  });
});

test("does not list paths through a directory symlink outside cwd", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-files-symlink-root-"));
  const outside = await mkdtemp(join(tmpdir(), "agent-harness-files-symlink-outside-"));
  try {
    await writeFile(join(outside, "secret-name.txt"), "secret\n");
    try {
      await symlink(
        outside,
        join(root, "external"),
        process.platform === "win32" ? "junction" : "dir",
      );
    } catch {
      t.skip("directory symlinks are unavailable on this platform");
      return;
    }
    const listing = await collectSessionFiles(root, LISTING, OPEN_POLICY);
    assert.deepEqual(listing, { files: [], truncated: false });
  } finally {
    await Promise.all([
      rm(root, { recursive: true, force: true }),
      rm(outside, { recursive: true, force: true }),
    ]);
  }
});

test("include_hidden reaches dot-files and dot-directories but still skips symlinks", async () => {
  await withTree(async (root) => {
    const listing = await collectSessionFiles(root, { ...LISTING, include_hidden: true }, OPEN_POLICY);
    // Breadth-first still applies: `.hidden/` is a second-level directory and
    // sorts before `src/`, so its file is listed before `src/index.ts`.
    assert.deepEqual(listing.files, [
      ".env",
      "README.md",
      ".hidden/secret.txt",
      "src/index.ts",
      "src/components/Button.tsx",
    ]);
    assert.equal(listing.files.includes("link.md"), false);
  });
});

test("an empty ignore list walks the directories the defaults skip", async () => {
  await withTree(async (root) => {
    const listing = await collectSessionFiles(root, { ...LISTING, ignore_directories: [] }, OPEN_POLICY);
    assert.ok(listing.files.includes("node_modules/pkg/index.js"));
  });
});

test("a deny rule in the read policy removes the files it covers", async () => {
  await withTree(async (root) => {
    const policy: NativePathPolicy = {
      paths: [{ action: "deny", path: join(root, "src") }],
      unrestricted_paths: false,
    };
    const listing = await collectSessionFiles(root, LISTING, policy);
    assert.deepEqual(listing.files, ["README.md"]);
  });
});

test("max_depth stops the walk and reports unvisited directories", async () => {
  await withTree(async (root) => {
    const listing = await collectSessionFiles(root, { ...LISTING, max_depth: 1 }, OPEN_POLICY);
    assert.deepEqual(listing.files, ["README.md"]);
    assert.equal(listing.truncated, true);
  });
});

test("a zero depth cap reports that the root was not walked", async () => {
  await withTree(async (root) => {
    const listing = await collectSessionFiles(root, { ...LISTING, max_depth: 0 }, OPEN_POLICY);
    assert.deepEqual(listing, { files: [], truncated: true });
  });
});

test("a results cap truncates the list instead of failing it", async () => {
  await withTree(async (root) => {
    const listing = await collectSessionFiles(root, { ...LISTING, max_results: 1 }, OPEN_POLICY);
    assert.deepEqual(listing.files, ["README.md"]);
    assert.equal(listing.truncated, true);
  });
});

test("an entry cap truncates without visiting the rest of the tree", async () => {
  await withTree(async (root) => {
    const listing = await collectSessionFiles(root, { ...LISTING, max_entries: 1 }, OPEN_POLICY);
    assert.equal(listing.truncated, true);
    assert.ok(listing.files.length <= 1);
  });
});

test("max_entries counts hidden entries that are omitted from the result", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-files-hidden-cap-"));
  try {
    await Promise.all(
      Array.from({ length: 20 }, (_, index) =>
        writeFile(join(root, `.hidden-${index}.txt`), "hidden\n"),
      ),
    );
    const listing = await collectSessionFiles(
      root,
      { ...LISTING, max_depth: 1, max_entries: 1 },
      OPEN_POLICY,
    );
    assert.deepEqual(listing, { files: [], truncated: true });
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("an exact entry cap does not report truncation when the directory is exhausted", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-files-exact-cap-"));
  try {
    await writeFile(join(root, "only.txt"), "only\n");
    const listing = await collectSessionFiles(
      root,
      { ...LISTING, max_depth: 1, max_entries: 1 },
      OPEN_POLICY,
    );
    assert.deepEqual(listing, { files: ["only.txt"], truncated: false });
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("a missing directory answers with an empty listing rather than an error", async () => {
  await withTree(async (root) => {
    const listing = await collectSessionFiles(join(root, "gone"), LISTING, OPEN_POLICY);
    assert.deepEqual(listing, { files: [], truncated: false });
  });
});

/** A session whose cwd is a workspace next to it, so the walk has something to list. */
async function writeFilesSession(
  root: string,
  sessionFiles?: Record<string, unknown>,
  readPolicy?: unknown,
): Promise<string> {
  const sessionDir = join(root, "session1");
  const workspace = join(root, "workspace");
  await mkdir(sessionDir, { recursive: true });
  await mkdir(join(workspace, "src"), { recursive: true });
  await writeFile(join(workspace, "README.md"), "# readme\n");
  await writeFile(join(workspace, "src/app.ts"), "export {};\n");
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      schema_version: 1,
      session_name: "files-test",
      cwd: workspace,
      ...(sessionFiles === undefined ? {} : { session_files: sessionFiles }),
      ...(readPolicy === undefined ? {} : { tools: { native: { read: readPolicy } } }),
    }),
  );
  return sessionDir;
}

async function withServer(
  root: string,
  run: (baseUrl: string) => Promise<void>,
): Promise<void> {
  const running = createAgentServer({ sessionsDir: root });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });
  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    await run(`http://127.0.0.1:${address.port}`);
  } finally {
    await running.close();
  }
}

test("the files endpoint answers with the session cwd listing", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-files-route-"));
  try {
    await writeFilesSession(root);
    await withServer(root, async (baseUrl) => {
      const response = await fetch(`${baseUrl}/api/sessions/session1/files`);
      assert.equal(response.status, 200);
      assert.deepEqual(await response.json(), {
        files: ["README.md", "src/app.ts"],
        truncated: false,
      });
    });
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("the files endpoint applies the session's read policy", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-files-policy-"));
  try {
    const sessionDir = await writeFilesSession(root);
    const workspace = join(root, "workspace");
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        schema_version: 1,
        session_name: "files-test",
        cwd: workspace,
        tools: { native: { read: { paths: [{ action: "deny", path: join(workspace, "src") }] } } },
      }),
    );
    await withServer(root, async (baseUrl) => {
      const response = await fetch(`${baseUrl}/api/sessions/session1/files`);
      assert.equal(response.status, 200);
      const body = (await response.json()) as { files: string[] };
      assert.deepEqual(body.files, ["README.md"]);
    });
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("a disabled listing answers 403 so the menu can fall back", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-files-off-"));
  try {
    await writeFilesSession(root, { enabled: false });
    await withServer(root, async (baseUrl) => {
      const response = await fetch(`${baseUrl}/api/sessions/session1/files`);
      assert.equal(response.status, 403);
    });
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("a session reference naming a root that does not exist answers 404", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-files-missing-"));
  try {
    await writeFilesSession(root);
    await withServer(root, async (baseUrl) => {
      // A single session root resolves any bare id to a fresh session directory
      // (the same rule every other route follows), so the 404 path is a root id
      // that is not configured. Rooted references read `root:session`.
      const response = await fetch(`${baseUrl}/api/sessions/unknown-root%3Asession1/files`);
      assert.equal(response.status, 404);
    });
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("session_files defaults come from configuration, not the file", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-files-defaults-"));
  try {
    await writeFilesSession(root, { max_depth: 1 });
    await withServer(root, async (baseUrl) => {
      const response = await fetch(`${baseUrl}/api/sessions/session1/files`);
      assert.equal(response.status, 200);
      assert.deepEqual(await response.json(), { files: ["README.md"], truncated: true });
    });
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
