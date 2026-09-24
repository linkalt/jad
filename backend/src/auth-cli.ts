#!/usr/bin/env node

import { access, readFile, unlink } from "node:fs/promises";
import { stdin, stdout } from "node:process";
import { createInterface } from "node:readline/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

import { runOpenAIOAuthLogin } from "openai-oauth";

import { resolveOpenAIOAuthAuthFilePath } from "./config.js";

const USAGE = `Usage: npm run auth -- <login|status|clear> [options]

Commands:
  login                 Sign in and save credentials locally
  status                Check whether a credential file is present
  clear                 Delete the local credential file (does not revoke OAuth)

Options:
  --oauth-file <path>   Auth file (default: ~/.jad/auth/openai-codex.json)
  --no-open             Print the sign-in URL instead of opening a browser
  --timeout-ms <ms>     Login timeout
  --force               Skip confirmation when replacing or clearing credentials
  --help                Show this help`;

interface AuthCliOptions {
  command: string;
  authFile?: string;
  openBrowser: boolean;
  timeoutMs?: number;
  force: boolean;
}

function parseArgs(args: string[]): AuthCliOptions {
  const firstArgument = args[0] ?? "help";
  const command = firstArgument === "--help" || firstArgument === "-h"
    ? "help"
    : firstArgument;
  const options: AuthCliOptions = {
    command,
    openBrowser: true,
    force: false,
  };

  for (let index = 1; index < args.length; index += 1) {
    const arg = args[index];
    if (arg === "--help" || arg === "-h") {
      options.command = "help";
      continue;
    }
    if (arg === "--oauth-file" || arg === "--auth-file") {
      const value = args[index + 1];
      if (!value || value.startsWith("--")) {
        throw new Error(`${arg} requires a path`);
      }
      options.authFile = value;
      index += 1;
      continue;
    }
    if (arg === "--no-open") {
      options.openBrowser = false;
      continue;
    }
    if (arg === "--force") {
      options.force = true;
      continue;
    }
    if (arg === "--timeout-ms") {
      const value = args[index + 1];
      const timeoutMs = Number(value);
      if (!value || !Number.isSafeInteger(timeoutMs) || timeoutMs <= 0) {
        throw new Error("--timeout-ms must be a positive safe integer");
      }
      options.timeoutMs = timeoutMs;
      index += 1;
      continue;
    }
    throw new Error(`unknown option: ${arg}`);
  }

  if (!["login", "status", "clear", "help"].includes(options.command)) {
    throw new Error(`unknown command: ${options.command}`);
  }
  return options;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

async function fileExists(path: string): Promise<boolean> {
  try {
    await access(path);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return false;
    }
    throw error;
  }
}

async function confirm(message: string, force: boolean): Promise<boolean> {
  if (force) return true;
  if (!stdin.isTTY || !stdout.isTTY) {
    throw new Error("confirmation is required in a terminal; pass --force to proceed");
  }
  const readline = createInterface({input: stdin, output: stdout});
  try {
    const answer = await readline.question(`${message} [y/N] `);
    return answer.trim().toLowerCase() === "y" || answer.trim().toLowerCase() === "yes";
  } finally {
    readline.close();
  }
}

async function showStatus(authFilePath: string): Promise<void> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(await readFile(authFilePath, "utf8"));
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      console.log(`No OAuth credential file found at ${authFilePath}`);
      return;
    }
    throw new Error(`Unable to read OAuth credential file: ${error instanceof Error ? error.message : String(error)}`);
  }

  const tokens = isRecord(parsed) && isRecord(parsed.tokens) ? parsed.tokens : undefined;
  const present = [tokens?.access_token, tokens?.refresh_token]
    .some((token) => typeof token === "string" && token.length > 0);
  console.log(present
    ? `OAuth credential file is present at ${authFilePath}`
    : `No OAuth tokens were found in ${authFilePath}`);
}

async function run(args = process.argv.slice(2)): Promise<void> {
  const options = parseArgs(args);
  if (options.command === "help") {
    console.log(USAGE);
    return;
  }

  const authFilePath = resolveOpenAIOAuthAuthFilePath(options.authFile);
  if (options.command === "status") {
    await showStatus(authFilePath);
    return;
  }

  if (options.command === "clear") {
    if (!(await fileExists(authFilePath))) {
      console.log(`No OAuth credential file found at ${authFilePath}`);
      return;
    }
    if (!(await confirm(`Delete local OAuth credentials at ${authFilePath}?`, options.force))) {
      console.log("Cancelled.");
      return;
    }
    await unlink(authFilePath);
    console.log("Local OAuth credential file deleted. This does not revoke the OAuth grant.");
    return;
  }

  if (await fileExists(authFilePath)) {
    if (!(await confirm(`Replace OAuth credentials at ${authFilePath}?`, options.force))) {
      console.log("Cancelled.");
      return;
    }
  }

  // The upstream package stores the token file with mode 0600. Also restrict
  // any newly created parent directories to the current user.
  process.umask(0o077);
  const result = await runOpenAIOAuthLogin({
    authFilePath,
    openBrowser: options.openBrowser,
    ...(options.timeoutMs === undefined ? {} : {timeoutMs: options.timeoutMs}),
    onMessage: (message) => console.log(message),
  });
  console.log(`OAuth login complete. Credentials saved to ${result.path}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  void run().catch((error: unknown) => {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  });
}
