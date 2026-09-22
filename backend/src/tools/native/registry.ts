/**
 * @file registry.ts
 * @input Session config
 * @output Exports the native tool specs consumed by the tool catalog
 * @position Single place that knows which native tools exist and how they load
 *
 * Adding a native tool means adding one spec here plus its definition document
 * entry; the catalog itself stays free of per-tool branches.
 */

import type {ValidateFunction} from "ajv";

import type {LoadedSessionConfig} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import type {LoadedTool, ToolDefinition} from "../types.js";
import {executeNativeBash, NATIVE_BASH_ID, nativeBashDefinition} from "./bash.js";
import {executeNativeEdit, NATIVE_EDIT_ID, nativeEditDefinition} from "./edit.js";
import {executeNativeGlob, NATIVE_GLOB_ID, nativeGlobDefinition} from "./glob.js";
import {executeNativeGrep, NATIVE_GREP_ID, nativeGrepDefinition} from "./grep.js";
import {executeNativeRead, NATIVE_READ_ID, nativeReadDefinition} from "./read.js";
import {executeNativeProcess, NATIVE_PROCESS_ID, nativeProcessDefinition, normalizeProcessArguments} from "./process.js";
import {ProcessManager} from "./process-manager.js";
import {executeNativeTask, NATIVE_TASK_ID, nativeTaskDefinition, normalizeTaskArguments} from "./task.js";
import {TaskStore} from "./task-store.js";
import {normalizeEncoding, normalizeRangeType} from "./text.js";
import {executeNativeWrite, NATIVE_WRITE_ID, nativeWriteDefinition} from "./write.js";

export interface NativeToolSpec {
  readonly id: string;
  /** Key under `tools.native` that enables the tool. */
  readonly settingName: string;
  readonly definition: ToolDefinition;
  /** Required input fields, in the order the definition must declare them. */
  readonly required: readonly string[];
  readonly exampleOperation: JsonRecord;
  /**
   * Tolerant pre-validation normalization: turns near-miss spellings the model
   * produces ("utf-8", "Lines") into the canonical schema values, so a
   * harmless variant does not cost a validation error and a retry.
   */
  normalizeArguments(args: JsonRecord): JsonRecord;
  configured(config: LoadedSessionConfig): boolean;
  build(
    definition: ToolDefinition,
    config: LoadedSessionConfig,
    validate: ValidateFunction<unknown>,
    resources?: {processManager?: ProcessManager; taskStore?: TaskStore},
  ): {tool: LoadedTool} | {error: string};
}

function readSpec(): NativeToolSpec {
  return {
    id: NATIVE_READ_ID,
    settingName: "read",
    definition: nativeReadDefinition,
    required: ["path"],
    exampleOperation: {path: "."},
    normalizeArguments: (args) => {
      const normalized = {...args};
      const encoding = normalizeEncoding(args.contentEncoding);
      if (encoding) {
        normalized.contentEncoding = encoding;
      }
      const rangeType = normalizeRangeType(args.rangeType);
      if (rangeType) {
        normalized.rangeType = rangeType;
      }
      return normalized;
    },
    configured: (config) => config.settings.tools.native.read !== undefined,
    build: (definition, config, validate) => {
      const policy = config.settings.tools.native.read;
      if (!policy) {
        return {error: `tool ${definition.id} is not exposed in tools.native`};
      }
      return {
        tool: {
          definition,
          validate,
          execute: (args, context) => executeNativeRead(args, {...context, policy}),
        },
      };
    },
  };
}

function writeSpec(): NativeToolSpec {
  return {
    id: NATIVE_WRITE_ID,
    settingName: "write",
    definition: nativeWriteDefinition,
    required: ["path", "content"],
    exampleOperation: {path: "example.txt", content: "..."},
    normalizeArguments: (args) => {
      const normalized = {...args};
      const encoding = normalizeEncoding(args.contentEncoding);
      if (encoding) {
        normalized.contentEncoding = encoding;
      }
      return normalized;
    },
    configured: (config) => config.settings.tools.native.write !== undefined,
    build: (definition, config, validate) => {
      const policy = config.settings.tools.native.write;
      if (!policy) {
        return {error: `tool ${definition.id} is not exposed in tools.native`};
      }
      return {
        tool: {
          definition,
          validate,
          execute: (args, context) => executeNativeWrite(args, {...context, policy}),
        },
      };
    },
  };
}

function editSpec(): NativeToolSpec {
  return {
    id: NATIVE_EDIT_ID,
    settingName: "edit",
    definition: nativeEditDefinition,
    required: ["path", "oldText", "newText"],
    exampleOperation: {path: "example.txt", oldText: "before", newText: "after"},
    normalizeArguments: (args) => args,
    configured: (config) => config.settings.tools.native.edit !== undefined,
    build: (definition, config, validate) => {
      const policy = config.settings.tools.native.edit;
      if (!policy) {
        return {error: `tool ${definition.id} is not exposed in tools.native`};
      }
      return {
        tool: {
          definition,
          validate,
          execute: (args, context) => executeNativeEdit(args, {...context, policy}),
        },
      };
    },
  };
}

function grepSpec(): NativeToolSpec {
  return {
    id: NATIVE_GREP_ID,
    settingName: "grep",
    definition: nativeGrepDefinition,
    required: ["pattern"],
    exampleOperation: {pattern: "TODO", path: "src"},
    normalizeArguments: (args) => args,
    configured: (config) => config.settings.tools.native.grep !== undefined,
    build: (definition, config, validate) => {
      const policy = config.settings.tools.native.grep;
      if (!policy) {
        return {error: `tool ${definition.id} is not exposed in tools.native`};
      }
      return {
        tool: {
          definition,
          validate,
          execute: (args, context) => executeNativeGrep(args, {...context, policy}),
        },
      };
    },
  };
}

export function nativeToolSpecs(): NativeToolSpec[] {
  return [readSpec(), writeSpec(), editSpec(), grepSpec(), bashSpec(), globSpec(), processSpec(), taskSpec()];
}

function taskSpec(): NativeToolSpec {
  return {
    id: NATIVE_TASK_ID,
    settingName: "task",
    definition: nativeTaskDefinition,
    required: [],
    exampleOperation: {title: "Plan the next change"},
    normalizeArguments: normalizeTaskArguments,
    configured: (config) => config.settings.tools.native.task !== undefined,
    build: (definition, config, validate, resources) => {
      const policy = config.settings.tools.native.task;
      if (!policy) return {error: `tool ${definition.id} is not exposed in tools.native`};
      const store = resources?.taskStore ?? new TaskStore(policy.storage_path);
      return {
        tool: {
          definition,
          validate,
          execute: (args, context) => executeNativeTask(args, {...context, policy, store}),
        },
      };
    },
  };
}

function processSpec(): NativeToolSpec {
  return {
    id: NATIVE_PROCESS_ID,
    settingName: "process",
    definition: nativeProcessDefinition,
    required: [],
    exampleOperation: {command: "npm run dev"},
    normalizeArguments: normalizeProcessArguments,
    configured: (config) => config.settings.tools.native.process !== undefined,
    build: (definition, config, validate, resources) => {
      const policy = config.settings.tools.native.process;
      if (!policy) return {error: `tool ${definition.id} is not exposed in tools.native`};
      const manager = resources?.processManager ?? new ProcessManager(policy.storage_dir);
      return {
        tool: {
          definition,
          validate,
          execute: (args, context) => executeNativeProcess(args, {
            ...context,
            policy,
            manager,
            apiKeyEnv: config.model.api_key_env,
          }),
        },
      };
    },
  };
}

function globSpec(): NativeToolSpec {
  return {
    id: NATIVE_GLOB_ID,
    settingName: "glob",
    definition: nativeGlobDefinition,
    required: [],
    exampleOperation: {pattern: "*.test.ts"},
    normalizeArguments: (args) => {
      // "glob" and "include" are common near-miss spellings of "pattern".
      const normalized = {...args};
      if (typeof normalized.pattern === "string") {
        normalized.pattern = normalized.pattern.trim();
      } else {
        const alias = args.glob ?? args.include;
        if (typeof alias === "string") {
          normalized.pattern = alias.trim();
        }
      }
      if (normalized.path === undefined && typeof args.dir === "string") {
        normalized.path = args.dir;
      }
      delete normalized.glob;
      delete normalized.include;
      delete normalized.dir;
      return normalized;
    },
    configured: (config) => config.settings.tools.native.glob !== undefined,
    build: (definition, config, validate) => {
      const policy = config.settings.tools.native.glob;
      if (!policy) {
        return {error: `tool ${definition.id} is not exposed in tools.native`};
      }
      return {
        tool: {
          definition,
          validate,
          execute: (args, context) => executeNativeGlob(args, {...context, policy}),
        },
      };
    },
  };
}

function bashSpec(): NativeToolSpec {
  return {
    id: NATIVE_BASH_ID,
    settingName: "bash",
    definition: nativeBashDefinition,
    required: ["command"],
    exampleOperation: {command: "npm test"},
    normalizeArguments: (args) => {
      // "cmd", "workdir", and "dir" are common near-miss spellings; normalize
      // them so a harmless variant does not cost a validation error and retry.
      const normalized = {...args};
      if (typeof normalized.command === "string") {
        normalized.command = normalized.command.trim();
      } else if (typeof args.cmd === "string") {
        normalized.command = args.cmd.trim();
      }
      if (normalized.cwd === undefined) {
        const alias = args.workdir ?? args.dir;
        if (typeof alias === "string") {
          normalized.cwd = alias;
        }
      }
      delete normalized.cmd;
      delete normalized.workdir;
      delete normalized.dir;
      return normalized;
    },
    configured: (config) => config.settings.tools.native.bash !== undefined,
    build: (definition, config, validate) => {
      const policy = config.settings.tools.native.bash;
      if (!policy) {
        return {error: `tool ${definition.id} is not exposed in tools.native`};
      }
      // The harness credential is always removed from the child environment, so
      // the tool needs to know which variable holds it.
      const apiKeyEnv = config.model.api_key_env;
      return {
        tool: {
          definition,
          validate,
          execute: (args, context) =>
            executeNativeBash(args, {...context, policy, apiKeyEnv}),
        },
      };
    },
  };
}
