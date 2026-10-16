import {type SessionSnapshot} from './api';
import {type Session} from './app-types';
import {oneLine} from './row-text';
import {sessionUiStateForRun} from './session-run-status';

export function sessionUiId(snapshot: SessionSnapshot, current: readonly Session[]): string {
  const sampleCollision = current.some(
    session => session.source === 'sample' && session.id === snapshot.id,
  );
  return sampleCollision ? `live:${snapshot.id}` : snapshot.id;
}

export function sessionFromSnapshot(snapshot: SessionSnapshot, id: string): Session {
  return {
    id,
    title: oneLine(snapshot.session_name) || snapshot.session_id || snapshot.id,
    time: snapshot.turns > 0 ? 'Local transcript' : 'Ready',
    turns: snapshot.turns,
    ...(snapshot.profile_turns === undefined ? {} : {profileTurns: snapshot.profile_turns}),
    state: sessionUiStateForRun(snapshot.run?.status),
    source: 'live',
    sessionRef: snapshot.id,
    ...(snapshot.runs_by_profile === undefined
      ? {}
      : {runsByProfile: snapshot.runs_by_profile}),
    ...(snapshot.root_label ? {rootLabel: snapshot.root_label} : {}),
    ...(snapshot.category ? {category: snapshot.category} : {}),
    ...(snapshot.active_profile === undefined ? {} : {activeProfile: snapshot.active_profile}),
    ...(snapshot.profiles === undefined ? {} : {profiles: snapshot.profiles}),
    ...(snapshot.profile_names === undefined ? {} : {profileNames: snapshot.profile_names}),
    ...(snapshot.model_default === undefined ? {} : {modelDefault: snapshot.model_default}),
    ...(snapshot.models === undefined ? {} : {models: snapshot.models}),
    ...(snapshot.front_compaction === undefined
      ? {}
      : {frontCompaction: snapshot.front_compaction}),
    model: snapshot.model,
    ...(snapshot.usage ? {usage: snapshot.usage} : {}),
    ...(snapshot.run ? {run: snapshot.run} : {}),
  };
}
