/**
 * @file checkpoint-git.ts
 * @input A workspace directory, a private git directory, a git executable
 * @output ShadowGit: snapshot the workspace as a tree, diff two trees, check paths out of a tree
 * @position Lowest layer of run checkpoints; knows git, nothing about sessions or runs
 *
 * The workspace is the work tree of a bare repository that lives elsewhere, and
 * every command names its own index file. The user's repository (its index,
 * HEAD, refs and stash) is therefore never read or written, and a workspace
 * that is not a git repository works the same way.
 */

import {spawn} from "node:child_process";
import {existsSync} from "node:fs";
import {mkdir, writeFile} from "node:fs/promises";
import {join} from "node:path";

export type GitFailure = "unavailable" | "timeout" | "failed";

export class GitError extends Error {
  public constructor(
    public readonly failure: GitFailure,
    message: string,
  ) {
    super(message);
    this.name = "GitError";
  }
}

/** One path that differs between two trees. `A` and `D` are relative to the first tree. */
export interface TreeChange {
  path: string;
  status: "A" | "D" | "M" | "T";
  oldMode: string;
  newMode: string;
  oldSha: string;
  newSha: string;
}

export interface ShadowGitOptions {
  gitDir: string;
  workTree: string;
  binary: string;
  timeoutMs: number;
  /** Largest output one git command may return before it is treated as a failure. */
  maxOutputBytes: number;
}

const SNAPSHOT_INDEX = "snapshot.index";
const RESTORE_INDEX = "restore.index";

/** Configuration that keeps a snapshot independent of the user's and the machine's git setup. */
const FIXED_CONFIG = [
  "core.autocrlf=false",
  "core.safecrlf=false",
  "core.fileMode=true",
  "core.symlinks=true",
  "core.fsmonitor=false",
  "core.hooksPath=/dev/null",
  "gc.auto=0",
  "maintenance.auto=false",
];

export class ShadowGit {
  public constructor(private readonly options: ShadowGitOptions) {}

  private env(indexFile?: string, bindRepository = true): NodeJS.ProcessEnv {
    const env: NodeJS.ProcessEnv = {...process.env};
    for (const name of ["GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE"]) {
      delete env[name];
    }
    // `git init` refuses a work tree it was not asked for, so it runs unbound.
    if (bindRepository) {
      env.GIT_DIR = this.options.gitDir;
      env.GIT_WORK_TREE = this.options.workTree;
    }
    if (indexFile !== undefined) env.GIT_INDEX_FILE = join(this.options.gitDir, indexFile);
    env.GIT_CONFIG_GLOBAL = "/dev/null";
    env.GIT_CONFIG_NOSYSTEM = "1";
    env.GIT_TERMINAL_PROMPT = "0";
    env.GIT_OPTIONAL_LOCKS = "0";
    env.GIT_AUTHOR_NAME = "agent-checkpoint";
    env.GIT_AUTHOR_EMAIL = "checkpoint@localhost";
    env.GIT_COMMITTER_NAME = "agent-checkpoint";
    env.GIT_COMMITTER_EMAIL = "checkpoint@localhost";
    return env;
  }

  private run(args: string[], options: {index?: string; input?: Buffer; unbound?: boolean} = {}): Promise<Buffer> {
    const full = FIXED_CONFIG.flatMap((entry) => ["-c", entry]).concat(args);
    return new Promise((resolve, reject) => {
      const child = spawn(this.options.binary, full, {
        cwd: this.options.workTree,
        env: this.env(options.index, options.unbound !== true),
        stdio: ["pipe", "pipe", "pipe"],
      });
      const out: Buffer[] = [];
      const err: Buffer[] = [];
      let outBytes = 0;
      let settled = false;
      const finish = (action: () => void): void => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        action();
      };
      const timer = setTimeout(() => {
        child.kill("SIGKILL");
        finish(() => reject(new GitError("timeout", `git ${args[0]} did not finish within ${this.options.timeoutMs} ms`)));
      }, this.options.timeoutMs);
      child.stdout.on("data", (chunk: Buffer) => {
        outBytes += chunk.length;
        if (outBytes > this.options.maxOutputBytes) {
          child.kill("SIGKILL");
          finish(() => reject(new GitError("failed", `git ${args[0]} returned more than ${this.options.maxOutputBytes} bytes`)));
          return;
        }
        out.push(chunk);
      });
      child.stderr.on("data", (chunk: Buffer) => {
        if (err.reduce((total, part) => total + part.length, 0) < 4096) err.push(chunk);
      });
      child.on("error", (error: NodeJS.ErrnoException) => {
        finish(() => reject(new GitError(
          error.code === "ENOENT" || error.code === "EACCES" ? "unavailable" : "failed",
          `cannot run ${this.options.binary}: ${error.message}`,
        )));
      });
      child.on("close", (code) => {
        finish(() => {
          if (code === 0) {
            resolve(Buffer.concat(out));
            return;
          }
          const detail = Buffer.concat(err).toString("utf8").trim().slice(0, 500);
          reject(new GitError("failed", `git ${args[0]} exited with ${code}${detail.length > 0 ? `: ${detail}` : ""}`));
        });
      });
      child.stdin.on("error", () => undefined);
      child.stdin.end(options.input);
    });
  }

  /** Creates the private repository on first use and writes the exclusion list. */
  public async prepare(excludePatterns: readonly string[]): Promise<void> {
    if (!existsSync(join(this.options.gitDir, "HEAD"))) {
      await mkdir(this.options.gitDir, {recursive: true});
      await this.run(["init", "--bare", "--quiet", this.options.gitDir], {unbound: true});
    }
    await mkdir(join(this.options.gitDir, "info"), {recursive: true});
    await writeFile(join(this.options.gitDir, "info", "exclude"), `${excludePatterns.join("\n")}\n`, "utf8");
  }

  /** Stages the whole workspace in the private index and returns the tree and its file count. */
  public async snapshot(): Promise<{tree: string; fileCount: number}> {
    await this.run(["add", "--all", "--", "."], {index: SNAPSHOT_INDEX});
    const tree = (await this.run(["write-tree"], {index: SNAPSHOT_INDEX})).toString("utf8").trim();
    const listing = await this.run(["ls-files", "-z"], {index: SNAPSHOT_INDEX});
    let fileCount = 0;
    for (const byte of listing) if (byte === 0) fileCount += 1;
    return {tree, fileCount};
  }

  public async commit(tree: string, message: string): Promise<string> {
    return (await this.run(["commit-tree", tree, "-m", message])).toString("utf8").trim();
  }

  public async setRef(ref: string, commit: string): Promise<void> {
    await this.run(["update-ref", ref, commit]);
  }

  public async deleteRef(ref: string): Promise<void> {
    await this.run(["update-ref", "-d", ref]);
  }

  /** Bytes the private repository occupies: loose objects plus packs. */
  public async repositoryBytes(): Promise<number> {
    const text = (await this.run(["count-objects", "-v"])).toString("utf8");
    let kib = 0;
    for (const line of text.split("\n")) {
      const match = /^(size|size-pack|size-garbage):\s+(\d+)/.exec(line);
      if (match !== null) kib += Number(match[2]);
    }
    return kib * 1024;
  }

  /** Paths that differ between two trees, with the mode and blob of each side. */
  public async diff(from: string, to: string): Promise<TreeChange[]> {
    if (from === to) return [];
    const raw = await this.run(["diff-tree", "-r", "-z", "--no-renames", "--raw", "--no-abbrev", from, to]);
    const parts = raw.toString("utf8").split("\0");
    const changes: TreeChange[] = [];
    for (let index = 0; index + 1 < parts.length; index += 2) {
      const meta = parts[index];
      const path = parts[index + 1];
      if (meta === undefined || path === undefined || !meta.startsWith(":")) continue;
      const [oldMode = "", newMode = "", oldSha = "", newSha = "", status = ""] = meta.slice(1).split(" ");
      const letter = status[0];
      if (letter !== "A" && letter !== "D" && letter !== "M" && letter !== "T") continue;
      changes.push({path, status: letter, oldMode, newMode, oldSha, newSha});
    }
    return changes;
  }

  /** Writes `paths` of `tree` into the workspace, replacing what is there. */
  public async checkout(tree: string, paths: readonly string[]): Promise<void> {
    if (paths.length === 0) return;
    await this.run(["read-tree", tree], {index: RESTORE_INDEX});
    await this.run(["checkout-index", "--force", "-z", "--stdin"], {
      index: RESTORE_INDEX,
      input: Buffer.from(`${paths.join("\0")}\0`, "utf8"),
    });
  }
}
