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
