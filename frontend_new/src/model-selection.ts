import type {ModelOption, Session} from './app-types';

export interface RunModelChoice {
  modelId: string;
  effort?: string;
}

type ModelRecord = Pick<Session, 'id' | 'modelDefault' | 'modelFault' | 'models' | 'modelOptions' | 'model'>;

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

export function splitModelDefault(modelDefault: string | undefined): {modelId?: string; effort?: string} {
  if (modelDefault === undefined || modelDefault === '') return {};
  const at = modelDefault.lastIndexOf('@');
  if (at <= 0) return {modelId: modelDefault};
  const effort = modelDefault.slice(at + 1);
  const modelId = modelDefault.slice(0, at);
  return effort === '' ? {modelId} : {modelId, effort};
}

/** Why the session's configured model cannot be used, or undefined when it can. */
export function modelFaultOf(record: Pick<Session, 'modelFault'> | undefined): string | undefined {
  const fault = record?.modelFault;
  return fault === undefined || fault === '' ? undefined : fault;
}

/**
 * What stops a run that names no model of its own: the session's configured model cannot be used,
 * and the server runs no other model in its place. The user has to choose one (the drawer saves
 * it as the profile's default); the client never picks for them. A run that names a model itself,
 * through a command, is the user's explicit choice and is left to the server.
 */
export function runModelBlock(
  record: Pick<Session, 'modelFault'> | undefined,
  explicitModel: string | undefined,
): string | undefined {
  const fault = modelFaultOf(record);
  if (fault === undefined || explicitModel !== undefined) return undefined;
  return `The configured model cannot be used (${fault}). Choose a model in the model picker, or fix the file. The message was not sent.`;
}

/**
 * Display choice: the profile's model_default (`id@effort`), else the first offered model. A
 * session whose configured model cannot be used has no choice: showing another model would
 * name a run that will not happen (the server runs none in its place).
 */
export function resolveRunModelChoice(record: ModelRecord): {option?: ModelOption; effort?: string} {
  if (modelFaultOf(record) !== undefined) return {};
  const options = modelOptionsForSession(record);
  const {modelId, effort: defaultEffort} = splitModelDefault(record.modelDefault);
  const option = options.find(o => o.id === modelId) ?? options[0];
  if (option === undefined) return {};
  const efforts = option.efforts ?? [];
  const effort =
    option.id === modelId && defaultEffort !== undefined && efforts.includes(defaultEffort)
      ? defaultEffort
      : undefined;
  return {option, effort};
}

/**
 * The If-Match a model-default write sends: the profile tag of the record the pane shows, but
 * only when that record was read for the profile being written. A pinned pane whose own record
 * has not arrived shows the session's record, which is another profile's file.
 */
export function modelDefaultIfMatch(
  record: Pick<Session, 'profileEtag' | 'activeProfile'> | undefined,
  profile: string | undefined,
): string | undefined {
  if (record?.profileEtag === undefined) return undefined;
  if (profile !== undefined && record.activeProfile !== profile) return undefined;
  return record.profileEtag;
}
