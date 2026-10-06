import type {JsonRecord} from "./persistence.js";

/**
 * Raised when a run has used the `max_run_tokens` of its setting and the model
 * asked for another round. The message is written for the person who started the
 * run: it names only numbers the harness itself counted, so a client may see it.
 */
export class RunTokenLimitError extends Error {
  constructor(
    readonly used: number,
    readonly limit: number,
  ) {
    super(
      `The run stopped before its next round: it used ${used} tokens, which reached the max_run_tokens limit of ${limit}.`,
    );
    this.name = "RunTokenLimitError";
  }
}

function tokenCount(value: unknown): number | undefined {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0 ? value : undefined;
}

/**
 * Tokens one provider response reports: its `total_tokens`, or input plus output
 * when no total is given. A response without usable usage counts as 0, because the
 * harness only counts what the provider reported.
 */
export function responseTokens(response: JsonRecord): number {
  const usage = response.usage;
  if (usage === null || typeof usage !== "object" || Array.isArray(usage)) return 0;
  const fields = usage as JsonRecord;
  const total = tokenCount(fields.total_tokens);
  if (total !== undefined) return total;
  const input = tokenCount(fields.input_tokens);
  const output = tokenCount(fields.output_tokens);
  if (input === undefined || output === undefined) return 0;
  const sum = input + output;
  return Number.isSafeInteger(sum) ? sum : 0;
}
