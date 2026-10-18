import type {ModelOption, Session} from './app-types';

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

export interface RunModelChoice {
  sessionKey: string;
  modelId: string;
  effort?: string;
}

type ModelRecord = Pick<Session, 'id' | 'modelDefault' | 'models' | 'modelOptions' | 'model'>;

export function modelOptionsForSession(record: ModelRecord): ModelOption[] {
  if (record.modelOptions !== undefined && record.modelOptions.length > 0) return record.modelOptions;
  return (record.models ?? []).map(id => ({id, provider: record.model?.provider}));
}

export function filterModelOptions(options: readonly ModelOption[], query: string): ModelOption[] {
  const needle = query.trim().toLowerCase();
  if (needle === '') return [...options];
  return options.filter(
    option => option.id.toLowerCase().includes(needle) || (option.provider ?? '').toLowerCase().includes(needle),
  );
}

export function defaultEffortFor(option: ModelOption): string | undefined {
  const efforts = option.efforts ?? [];
  if (option.defaultEffort !== undefined && efforts.includes(option.defaultEffort)) return option.defaultEffort;
  return efforts[0];
}

/** One table row of the composer's model drawer. */
export interface ModelChoiceRow {
  option: ModelOption;
  isSelected: boolean;
  /** The effort this row marks: the run's pick for the selected model, else the option's default. */
  markedEffort?: string;
}

/**
 * The rows the model drawer shows. Collapsed, it is the running model alone —
 * clicking that row's id opens the list. Expanded, it is every model the active
 * session offers, filtered by the search text.
 */
export function modelChoiceRows(
  options: readonly ModelOption[],
  selectedModelId: string | undefined,
  selectedEffort: string | undefined,
  query: string,
  isExpanded: boolean,
): ModelChoiceRow[] {
  const visible = isExpanded
    ? filterModelOptions(options, query)
    : options.filter(option => option.id === (selectedModelId ?? options[0]?.id));
  return visible.map(option => {
    const isSelected = option.id === selectedModelId;
    const marked =
      isSelected && selectedEffort !== undefined && (option.efforts ?? []).includes(selectedEffort)
        ? selectedEffort
        : defaultEffortFor(option);
    return marked === undefined
      ? {option, isSelected}
      : {option, isSelected, markedEffort: marked};
  });
}

export function formatRunModel(id: string, effort?: string): string {
  const base = id.replace(/@[^@]*$/, '');
  return effort === undefined || effort === '' ? base : `${base}@${effort}`;
}

/** Display choice: the stored pick for this record's id, else the session default model. */
export function resolveRunModelChoice(
  record: ModelRecord,
  stored: RunModelChoice | null | undefined,
): {option?: ModelOption; effort?: string} {
  const options = modelOptionsForSession(record);
  const storedOption = stored?.sessionKey === record.id ? options.find(o => o.id === stored.modelId) : undefined;
  if (stored !== null && stored !== undefined && storedOption !== undefined) {
    const efforts = storedOption.efforts ?? [];
    const effort =
      stored.effort !== undefined && efforts.includes(stored.effort) ? stored.effort : defaultEffortFor(storedOption);
    return {option: storedOption, effort};
  }
  const defaultId = record.modelDefault?.replace(/@[^@]*$/, '');
  const option = options.find(o => o.id === defaultId) ?? options[0];
  return option === undefined ? {} : {option, effort: defaultEffortFor(option)};
}

export function resolveRunModelSelection(
  record: ModelRecord,
  stored: RunModelChoice | null | undefined,
): string | undefined {
  const {option, effort} = resolveRunModelChoice(record, stored);
  return option === undefined ? undefined : formatRunModel(option.id, effort);
}
