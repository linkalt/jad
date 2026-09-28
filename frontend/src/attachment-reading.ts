export interface TextPrefix {
  text: string;
  truncated: boolean;
}

/**
 * Read only enough UTF-8 bytes to produce `characterLimit + 1` UTF-16 code
 * units. A UTF-8 scalar uses at most four bytes, and a leading BOM uses three
 * more; the extra code unit tells us whether the original text was truncated.
 */
export async function readTextPrefix(
  file: Blob,
  characterLimit: number,
): Promise<TextPrefix> {
  if (!Number.isSafeInteger(characterLimit) || characterLimit < 0) {
    throw new RangeError('the attachment character limit must be a nonnegative safe integer');
  }
  const byteLimit = (characterLimit + 1) * 4 + 3;
  if (!Number.isSafeInteger(byteLimit)) {
    throw new RangeError('the attachment character limit is too large');
  }

  const text = await file.slice(0, byteLimit).text();
  return {
    text: text.slice(0, characterLimit),
    truncated: text.length > characterLimit,
  };
}

/** Read attachment prefixes with bounded concurrency, retaining picker order. */
export async function readAttachmentBlocks(
  files: readonly File[],
  characterLimit: number,
  maxConcurrentReads: number,
): Promise<string[]> {
  if (!Number.isSafeInteger(maxConcurrentReads) || maxConcurrentReads < 1) {
    throw new RangeError('attachment read concurrency must be a positive safe integer');
  }
  if (files.length === 0) {
    return [];
  }

  const blocks = new Array<string>(files.length);
  const workerCount = Math.min(files.length, maxConcurrentReads);
  let nextIndex = 0;
  let failure: {error: unknown} | undefined;

  const readNext = async (): Promise<void> => {
    while (failure === undefined) {
      const index = nextIndex;
      nextIndex += 1;
      if (index >= files.length) {
        return;
      }
      const file = files[index];
      if (file === undefined) {
        continue;
      }
      try {
        const prefix = await readTextPrefix(file, characterLimit);
        const body = prefix.truncated
          ? `${prefix.text}\n\n… truncated at ${characterLimit} characters`
          : prefix.text;
        blocks[index] = `**Attached: ${file.name}**\n\n` + '```\n' + body + '\n```';
      } catch (error) {
        failure = {error};
      }
    }
  };

  await Promise.all(Array.from({length: workerCount}, readNext));
  if (failure !== undefined) {
    throw failure.error;
  }
  return blocks;
}
