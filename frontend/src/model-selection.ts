export function selectedModelForSession(
  models: readonly string[] | undefined,
  modelDefault: string | undefined,
  fallbackModel: string | undefined,
): string | undefined {
  if (modelDefault !== undefined && (models === undefined || models.includes(modelDefault))) {
    return modelDefault;
  }
  return models?.[0] ?? fallbackModel;
}

/**
 * Filters session models for the composer's model picker: case-insensitive,
 * and every whitespace-separated term has to appear in the model name. An
 * empty query keeps the list untouched.
 */
export function filterModels(models: readonly string[], query: string): string[] {
  const terms = query.toLowerCase().split(/\s+/).filter(term => term.length > 0);
  if (terms.length === 0) return [...models];
  return models.filter(model => {
    const haystack = model.toLowerCase();
    return terms.every(term => haystack.includes(term));
  });
}
