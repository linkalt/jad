const MODEL_SELECTIONS_STORAGE_KEY = 'agent-harness-model-selections';

interface StorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

function browserStorage(): StorageLike | undefined {
  try {
    return typeof window === 'undefined' ? undefined : window.localStorage;
  } catch {
    return undefined;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export type ModelSelectionBySession = Record<string, string>;

export function loadModelSelectionBySession(
  storage: StorageLike | undefined = browserStorage(),
): ModelSelectionBySession {
  if (storage === undefined) {
    return {};
  }
  try {
    const value = storage.getItem(MODEL_SELECTIONS_STORAGE_KEY);
    if (value === null) {
      return {};
    }
    const parsed: unknown = JSON.parse(value);
    if (!isRecord(parsed)) {
      return {};
    }
    const selections: ModelSelectionBySession = {};
    for (const [sessionRef, model] of Object.entries(parsed)) {
      if (sessionRef.trim().length > 0 && typeof model === 'string' && model.trim().length > 0) {
        selections[sessionRef] = model.trim();
      }
    }
    return selections;
  } catch {
    return {};
  }
}

export function saveModelSelectionBySession(
  selections: Readonly<ModelSelectionBySession>,
  storage: StorageLike | undefined = browserStorage(),
): void {
  if (storage === undefined) {
    return;
  }
  try {
    storage.setItem(MODEL_SELECTIONS_STORAGE_KEY, JSON.stringify(selections));
  } catch {
    // Storage can be unavailable in private browsing or when the quota is full.
  }
}

export function selectedModelForSession(
  sessionRef: string | undefined,
  models: readonly string[] | undefined,
  modelDefault: string | undefined,
  fallbackModel: string | undefined,
  selections: Readonly<ModelSelectionBySession>,
): string | undefined {
  const selected = sessionRef === undefined ? undefined : selections[sessionRef];
  if (selected !== undefined && models?.includes(selected)) {
    return selected;
  }
  if (modelDefault !== undefined && (models === undefined || models.includes(modelDefault))) {
    return modelDefault;
  }
  return models?.[0] ?? fallbackModel;
}
