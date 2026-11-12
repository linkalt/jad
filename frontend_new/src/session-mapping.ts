import {parseModelOptions, type SessionSnapshot} from './api';
import {type Session} from './app-types';
import {oneLine} from './row-text';
import {sessionUiStateForRun} from './session-run-status';

export function sessionFromSnapshot(snapshot: SessionSnapshot, id: string): Session {
  const modelOptions = parseModelOptions(snapshot.model_options);
  return {
    id,
    title: oneLine(snapshot.session_name) || snapshot.session_id || snapshot.id,
    time: snapshot.turns > 0 ? 'Local transcript' : 'Ready',
    turns: snapshot.turns,
    ...(snapshot.profile_turns === undefined ? {} : {profileTurns: snapshot.profile_turns}),
    state: sessionUiStateForRun(snapshot.run?.status),
    sessionRef: snapshot.id,
    ...(snapshot.runs_by_profile === undefined
      ? {}
      : {runsByProfile: snapshot.runs_by_profile}),
    ...(snapshot.root_label ? {rootLabel: snapshot.root_label} : {}),
    ...(snapshot.category ? {category: snapshot.category} : {}),
    ...(snapshot.active_profile === undefined ? {} : {activeProfile: snapshot.active_profile}),
    ...(snapshot.default_profile === undefined ? {} : {defaultProfile: snapshot.default_profile}),
    ...(snapshot.profiles === undefined ? {} : {profiles: snapshot.profiles}),
    ...(snapshot.profile_names === undefined ? {} : {profileNames: snapshot.profile_names}),
    ...(snapshot.model_default === undefined ? {} : {modelDefault: snapshot.model_default}),
    ...(typeof snapshot.model_fault === 'string' && snapshot.model_fault !== ''
      ? {modelFault: snapshot.model_fault}
      : {}),
    ...(typeof snapshot.profile_etag === 'string' && snapshot.profile_etag !== '' ? {profileEtag: snapshot.profile_etag} : {}),
    ...(snapshot.models === undefined ? {} : {models: snapshot.models}),
    ...(modelOptions === undefined ? {} : {modelOptions}),
    ...(snapshot.front_compaction === undefined
      ? {}
      : {frontCompaction: snapshot.front_compaction}),
    model: snapshot.model,
    ...(snapshot.usage ? {usage: snapshot.usage} : {}),
    ...(snapshot.run ? {run: snapshot.run} : {}),
  };
}
