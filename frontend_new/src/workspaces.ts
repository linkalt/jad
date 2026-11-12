/**
 * @file workspaces.ts
 * @input The workspaces the server lists, the sessions' roots, and what the user picked
 * @output The workspaces the rail groups by, the scope a catalog request carries, and the
 *   reference a new session is created under
 * @position Pure rules behind the workspace rail, switcher and catalog scope; no React, no network
 *
 * A session's workspace is the session root it is stored in: `root_id` and `root_label` of
 * every list record. In the workspace layout each served workspace is one root; in the old
 * layout the roots of the setting are the workspaces. Only ids and labels are used here,
 * never a directory, and no session id or state path depends on any of it.
 */
import type {CatalogKind, Workspace} from './api.ts';

export type {Workspace} from './api.ts';

/** What a session says about its workspace. */
export interface WorkspaceSource {
  rootId?: string;
  rootLabel?: string;
}

/** The label of a session with no root at all (an older server), so it still has a place. */
export const UNKNOWN_WORKSPACE_LABEL = 'Unknown workspace';

/**
 * The workspaces a session can be listed under and a new one created in, start workspace
 * first, then the others by label.
 *
 * `listed` is GET /api/workspaces: empty outside the workspace layout, where the workspaces
 * are read from the sessions' roots instead (a root with no session is then not known). A
 * listed workspace the server does not serve is left out: it can hold no session and cannot
 * be targeted.
 */
export function workspacesOf(listed: readonly Workspace[], sessions: readonly WorkspaceSource[]): Workspace[] {
  const found = new Map<string, Workspace>();
  for (const workspace of listed) {
    if (workspace.unavailable === undefined) found.set(workspace.id, workspace);
  }
  for (const session of sessions) {
    const id = session.rootId ?? '';
    if (found.has(id)) continue;
    const label = session.rootLabel?.trim() || (id === '' ? UNKNOWN_WORKSPACE_LABEL : id);
    found.set(id, {id, label});
  }
  return [...found.values()].sort(
    (a, b) => Number(b.start === true) - Number(a.start === true) || a.label.localeCompare(b.label) || a.id.localeCompare(b.id),
  );
}

/** What the switcher says under a listed workspace the server does not serve. */
export const UNAVAILABLE_WORKSPACE_NOTE = 'Not available (the server log says why)';

/** One entry of the workspace switcher. */
export interface WorkspaceOption {
  value: string;
  label: string;
  /** A listed workspace the server does not serve: shown, but it cannot be chosen. */
  disabled?: true;
  description?: string;
}

/**
 * The switcher's entries: the served workspaces, then the listed ones that are not served,
 * greyed out, so a workspace never just disappears from the list. Only the id and label are
 * used. The server's reason is not shown: it is an error text that names files and folders,
 * and the screen shows no path. It is in the server log.
 */
export function workspaceOptions(workspaces: readonly Workspace[], listed: readonly Workspace[]): WorkspaceOption[] {
  const options: WorkspaceOption[] = workspaces.map(workspace => ({value: workspace.id, label: workspace.label}));
  const shown = new Set(workspaces.map(workspace => workspace.id));
  const down = listed
    .filter(workspace => workspace.unavailable !== undefined && !shown.has(workspace.id))
    .sort((a, b) => a.label.localeCompare(b.label) || a.id.localeCompare(b.id));
  for (const workspace of down) {
    options.push({value: workspace.id, label: workspace.label, disabled: true, description: UNAVAILABLE_WORKSPACE_NOTE});
  }
  return options;
}

/** Whether the server runs the workspace layout: only then is a scope or workspace sent. */
export function isWorkspaceLayout(listed: readonly Workspace[]): boolean {
  return listed.some(workspace => workspace.unavailable === undefined);
}

/** The workspace new sessions and catalog requests go to: the pick if still known, else the start one, else the first. */
export function effectiveWorkspace(workspaces: readonly Workspace[], picked: string | undefined): Workspace | undefined {
  return (
    workspaces.find(workspace => workspace.id === picked) ??
    workspaces.find(workspace => workspace.start === true) ??
    workspaces[0]
  );
}

/** One heading of the rail and the sessions under it. */
export interface WorkspaceSection<G> {
  /** Absent for the single flat list of a one-workspace setup, which has no heading. */
  workspace?: Workspace;
  groups: G[];
}

/**
 * Splits the grouped list by workspace, in the order of `workspaces`; sessions keep their list
 * order inside one. With one workspace there is one section without a heading: the switcher
 * and the pane already say where you are. A session whose root is not in `workspaces` gets
 * its own section at the end rather than vanishing.
 */
export function groupByWorkspace<G extends {workspace?: string}>(
  groups: readonly G[],
  workspaces: readonly Workspace[],
): WorkspaceSection<G>[] {
  if (workspaces.length <= 1) return groups.length === 0 ? [] : [{groups: [...groups]}];
  const sections = new Map<string, WorkspaceSection<G>>();
  for (const workspace of workspaces) sections.set(workspace.id, {workspace, groups: []});
  for (const group of groups) {
    const key = group.workspace ?? '';
    let section = sections.get(key);
    if (section === undefined) {
      section = {workspace: {id: key, label: key === '' ? UNKNOWN_WORKSPACE_LABEL : key}, groups: []};
      sections.set(key, section);
    }
    section.groups.push(group);
  }
  return [...sections.values()];
}

/** The session reference's root part (`<root>:<name>`), or undefined for a bare reference. */
export function workspaceOfSessionRef(sessionRef: string): string | undefined {
  const colon = sessionRef.indexOf(':');
  return colon > 0 ? sessionRef.slice(0, colon) : undefined;
}

/**
 * The workspace a library request of a session names: its own root, and only in the
 * workspace layout (a server outside it refuses the parameter).
 */
export function libraryWorkspace(layout: boolean, rootId: string | undefined): string | undefined {
  return layout && rootId !== undefined && rootId !== '' ? rootId : undefined;
}

/**
 * The scope a catalog request carries in the workspace layout, undefined outside it (the server
 * refuses a scope there) and for sessions (their reference names the workspace).
 *
 * Profiles always belong to a workspace. Models and toolsets are the global ones (`shared`
 * 'global', the default) or the workspace's own; MCP servers and sandbox templates are global.
 */
export function catalogScope(
  kind: CatalogKind,
  layout: boolean,
  workspace: string | undefined,
  shared: SharedScope,
): string | undefined {
  if (!layout || kind === 'sessions') return undefined;
  if (kind === 'profiles' || (shared === 'workspace' && (kind === 'models' || kind === 'toolsets'))) {
    return workspace === undefined ? undefined : `workspace:${workspace}`;
  }
  return 'global';
}

/** Where the models and toolsets of the catalog dialog are read and written. */
export type SharedScope = 'global' | 'workspace';

/** Whether the catalog kind has a choice between the global files and the workspace's own. */
export function hasSharedScopeChoice(kind: CatalogKind): boolean {
  return kind === 'models' || kind === 'toolsets';
}

/**
 * The reference a new session is created under. A name typed with its root (`<root>:<name>`)
 * is used as typed, so the user can always name the workspace; a bare name goes to the chosen
 * workspace when there is more than one (the server refuses a bare name then), and stays bare
 * with one.
 */
export function newSessionRef(typed: string, workspace: string | undefined, workspaceCount: number): string {
  const name = typed.trim();
  if (name === '' || name.includes(':') || workspaceCount <= 1 || workspace === undefined || workspace === '') return name;
  return `${workspace}:${name}`;
}

/**
 * The sentence under the new-session name: where the session will be created. Absent when
 * nothing is typed or there is one workspace (the generic hint says enough then).
 */
export function newSessionNote(typed: string, workspace: string | undefined, workspaces: readonly Workspace[]): string | undefined {
  const reference = newSessionRef(typed, workspace, workspaces.length);
  if (typed.trim() === '' || workspaces.length <= 1) return undefined;
  const root = workspaceOfSessionRef(reference);
  const label = workspaces.find(candidate => candidate.id === root)?.label ?? root;
  return label === undefined ? undefined : `Created as ${reference} in workspace “${label}”.`;
}
