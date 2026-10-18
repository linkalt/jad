/**
 * @file tool-result-view.ts
 * @input A tool result's output text and its image counts
 * @output Whether the result carries images, and the output text worth showing beside them
 * @position Used by the tool result views in App.tsx
 *
 * The server puts a fixed placeholder line in the output for every image part
 * ("[MCP image content]"). Once the images are shown that line only misleads, so
 * a result made of nothing else shows no text block.
 */

const IMAGE_PLACEHOLDER = '[MCP image content]';

export interface ToolResultImages {
  /** File names of images the server stored for the result. */
  images?: readonly string[];
  /** How many images of the result are still inline in the transcript. */
  inlineImages?: number;
}

/** True when the result has at least one image to show. */
export function hasToolResultImages({images, inlineImages}: ToolResultImages): boolean {
  return (images?.length ?? 0) > 0 || (inlineImages ?? 0) > 0;
}

/** The output text to show, or undefined when it is only image placeholders. */
export function toolResultText(output: string, result: ToolResultImages): string | undefined {
  if (!hasToolResultImages(result)) return output;
  const lines = output.split('\n').filter(line => line.trim() !== '' && line.trim() !== IMAGE_PLACEHOLDER);
  return lines.length === 0 ? undefined : lines.join('\n');
}
