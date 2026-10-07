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
import {executeNativeExec, NATIVE_EXEC_ID, nativeExecDefinition, normalizeExecArguments} from "./exec.js";
import {executeNativeEdit, NATIVE_EDIT_ID, nativeEditDefinition} from "./edit.js";
import {executeNativeGlob, NATIVE_GLOB_ID, nativeGlobDefinition} from "./glob.js";
import {executeNativeGrep, NATIVE_GREP_ID, nativeGrepDefinition, normalizeGrepArguments} from "./grep.js";
import {executeNativeRead, NATIVE_READ_ID, nativeReadDefinition} from "./read.js";
import {executeNativeProcess, NATIVE_PROCESS_ID, nativeProcessDefinition, normalizeProcessArguments} from "./process.js";
import {executeNativeFetch, NATIVE_FETCH_ID, nativeFetchDefinition, normalizeFetchArguments} from "./fetch.js";
import {FetchStore} from "./fetch-store.js";
import {executeNativeMemory, NATIVE_MEMORY_ID, nativeMemoryDefinition, normalizeMemoryArguments} from "./memory.js";
import {MemoryStore} from "./memory-store.js";
import {ProcessManager} from "./process-manager.js";
import {executeNativeTask, NATIVE_TASK_ID, nativeTaskDefinition, normalizeTaskArguments} from "./task.js";
import {TaskStore} from "./task-store.js";
import {executeNativeRequestDraft, NATIVE_REQUEST_DRAFT_ID, nativeRequestDraftDefinition} from "./request-draft.js";
import {executeNativeSkill, NATIVE_SKILL_ID, nativeSkillDefinition, normalizeSkillArguments} from "./skill.js";
import {normalizeEncoding, normalizeRangeType} from "./text.js";
import {executeNativeWrite, NATIVE_WRITE_ID, nativeWriteDefinition} from "./write.js";

export interface NativeToolSpec {
  readonly id: string;
  /** Key under `tools.native` that enables the tool. */
  readonly settingName: string;
  readonly definition: ToolDefinition;
  /** If set, the session resolves this call before executing co-batched calls. */
  readonly requiresUserDecision?: boolean;
  /**
   * True when a call never changes workspace or harness state and does not depend
   * on one that does, so it may run beside other calls of the same response.
   */
  readonly readOnly?: boolean;
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
    resources?: {processManager?: ProcessManager; taskStore?: TaskStore; memoryStore?: MemoryStore},
  ): {tool: LoadedTool} | {error: string};
}

function readSpec(): NativeToolSpec {
  return {
    id: NATIVE_READ_ID,
    readOnly: true,
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
      if (typeof args.offset === "number" && Number.isSafeInteger(args.offset)) {
        normalized.offset = Math.max(0, args.offset);
      }
      if (typeof args.limit === "number" && Number.isSafeInteger(args.limit)) {
        normalized.limit = Math.max(1, args.limit);
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
    readOnly: true,
    settingName: "grep",
    definition: nativeGrepDefinition,
    required: ["pattern"],
    exampleOperation: {pattern: "TODO", path: "src"},
    normalizeArguments: normalizeGrepArguments,
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
  return [readSpec(), writeSpec(), editSpec(), grepSpec(), bashSpec(), execSpec(), globSpec(), processSpec(), taskSpec(), memorySpec(), fetchSpec(), requestDraftSpec(), skillSpec()];
}

function skillSpec(): NativeToolSpec {
  return {
    id: NATIVE_SKILL_ID,
    readOnly: true,
    settingName: "skill",
    definition: nativeSkillDefinition,
    required: [],
    exampleOperation: {name: "agent-tool-design-guidelines"},
    normalizeArguments: normalizeSkillArguments,
    configured: (config) => config.settings.tools.native.skill !== undefined,
    build: (definition, config, validate) => {
      const policy = config.settings.tools.native.skill;
      if (!policy) {
        return {error: `tool ${definition.id} is not exposed in tools.native`};
      }
      const globalJadRoot = config.paths.global_jad_root;
      return {
        tool: {
          definition,
          validate,
          execute: (args, context) => executeNativeSkill(args, {...context, policy, globalJadRoot}),
        },
      };
    },
  };
}

/**
 * The draft tool has no storage and no path policy: its whole behaviour is the
 * question it publishes and the answer it reads back, so the only setting is
 * how long the run waits for that answer.
 */
function requestDraftSpec(): NativeToolSpec {
  return {
    id: NATIVE_REQUEST_DRAFT_ID,
    settingName: "request_draft",
    definition: nativeRequestDraftDefinition,
    requiresUserDecision: true,
    required: ["draft"],
    exampleOperation: {title: "How I read your request", draft: {goal: "..."}},
    normalizeArguments: (args) => args,
    configured: (config) => config.settings.tools.native.request_draft !== undefined,
    build: (definition, config, validate) => {
      const policy = config.settings.tools.native.request_draft;
      if (!policy) {
        return {error: `tool ${definition.id} is not exposed in tools.native`};
      }
      return {
        tool: {
          definition,
          validate,
          execute: (args, context) => executeNativeRequestDraft(args, context, policy),
        },
      };
    },
  };
}

function fetchSpec(): NativeToolSpec {
  return {
    id: NATIVE_FETCH_ID, settingName: "fetch", definition: nativeFetchDefinition,
    readOnly: true,
    required: [], exampleOperation: {url: "https://example.com"}, normalizeArguments: normalizeFetchArguments,
    configured: (config) => config.settings.tools.native.fetch !== undefined,
    build: (definition, config, validate) => {
      const policy = config.settings.tools.native.fetch;
      if (!policy) return {error: `tool ${definition.id} is not exposed in tools.native`};
      const store = new FetchStore(policy.storage_dir);
      return {tool: {definition, validate, execute: (args, context) => executeNativeFetch(args, {...context, policy, store})}};
    },
  };
}

function memorySpec(): NativeToolSpec {
  return {
    id: NATIVE_MEMORY_ID, settingName: "memory", definition: nativeMemoryDefinition,
    required: ["operation", "type"], exampleOperation: {operation: "list", type: "workspace"}, normalizeArguments: normalizeMemoryArguments,
    configured: (config) => config.settings.tools.native.memory !== undefined,
    build: (definition, config, validate, resources) => {
      const policy = config.settings.tools.native.memory;
      if (!policy) return {error: `tool ${definition.id} is not exposed in tools.native`};
      const store = resources?.memoryStore ?? new MemoryStore(policy.storage_dir);
      return {tool: {definition, validate, execute: (args, context) => executeNativeMemory(args, {...context, policy, store})}};
    },
  };
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
      const store = resources?.taskStore ?? new TaskStore(policy.storage_dir);
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
    readOnly: true,
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

function execSpec(): NativeToolSpec {
  return {
    id: NATIVE_EXEC_ID,
    settingName: "exec",
    definition: nativeExecDefinition,
    required: ["code"],
    exampleOperation: {code: "echo hello"},
    normalizeArguments: (args) => normalizeExecArguments(args),
    configured: (config) => config.settings.tools.native.exec !== undefined,
    build: (definition, config, validate) => {
      const exec = config.settings.tools.native.exec;
      const policy = config.settings.tools.native.bash;
      if (!exec || !policy) {
        return {error: `tool ${definition.id} is not exposed in tools.native`};
      }
      const apiKeyEnv = config.model.api_key_env;
      return {
        tool: {
          definition,
          validate,
          // The configured default language is only known here, not to the static spec.
          normalizeArguments: (args) => normalizeExecArguments(args, exec.default_lang),
          execute: (args, context) => executeNativeExec(args, {...context, policy, apiKeyEnv, exec}),
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
      // `input` is the other common name for text piped to a command.
      if (normalized.stdin === undefined && typeof args.input === "string") {
        normalized.stdin = args.input;
      }
      delete normalized.cmd;
      delete normalized.workdir;
      delete normalized.dir;
      delete normalized.input;
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
