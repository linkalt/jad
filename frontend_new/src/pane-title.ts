/**
 * What a pane's title strip says.
 *
 * A pane is a tab: it names the conversation it shows, then the profile that
 * conversation is pinned to, then the provider, the model and the reasoning
 * effort the next run uses. Anything unknown is left out rather than shown as a
 * placeholder, so a session without a model still reads as one clean title.
 */
import appConfig from './config.json' with {type: 'json'};

export type PaneTitlePartKey = 'session' | 'profile' | 'provider' | 'model' | 'effort';

export type PaneTitleInput = {
  sessionTitle: string;
  profile?: string;
  provider?: string;
  model?: string;
  effort?: string;
};

export type PaneTitlePart = {
  key: PaneTitlePartKey;
  label: string;
};

/**
 * Cut a part down to the configured length, keeping the head of the text: the
 * start of a model or session name is what tells two of them apart.
 */
export function truncatePaneTitlePart(
  value: string,
  limit: number = appConfig.pane_title_part_max_chars,
): string {
  if (limit <= 0 || value.length <= limit) return value;
  return `${value.slice(0, limit)}…`;
}

/** The visible parts of a pane's title, in reading order, without the empty ones. */
export function paneTitleParts(input: PaneTitleInput): PaneTitlePart[] {
  const parts: PaneTitlePart[] = [];
  const push = (key: PaneTitlePartKey, value: string | undefined): void => {
    const text = (value ?? '').trim();
    if (text === '') return;
    parts.push({key, label: truncatePaneTitlePart(text)});
  };
  push('session', input.sessionTitle);
  push('profile', input.profile);
  push('provider', input.provider);
  push('model', input.model);
  push('effort', input.effort);
  return parts;
}

/** The one-line form of the same title, for tooltips and screen readers. */
export function paneTitleText(input: PaneTitleInput): string {
  return paneTitleParts(input)
    .map(part => part.label)
    .join(' · ');
}
