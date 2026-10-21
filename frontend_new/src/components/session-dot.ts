import {colorVars} from '@astryxdesign/core/theme/tokens.stylex';
import * as stylex from '@stylexjs/stylex';
import type {DotMark} from '../session-views';

const styles = stylex.create({
  /** The session level's fill, painted over `StatusDot`'s own variant colour. */
  levelFill: {
    backgroundColor: colorVars['--color-icon-blue'],
  },
});

/**
 * The paint a dot wears, taken from its mark rather than from its variant.
 *
 * `StatusDot` has no blue variant, so a session label keeps the base variant
 * `neutral` and this overrides the fill. It lives in one place because both
 * components that draw a rail dot need it, and neither can own it: the rail and
 * the search panel are siblings, and the pure modules behind them
 * (`session-views`, `profile-search`) are imported directly by `node --test`,
 * where a StyleX import has no compiled form.
 *
 * `--color-icon-blue` and not a paler blue: `--color-background-blue` (#b9d7ff)
 * and `--color-border-blue` (#a1caff) measure 1.48:1 and 1.69:1 against the
 * white rail, where an 8px dot reads as an empty swatch. `--color-icon-blue`
 * measures 5.39:1 light and 7.70:1 dark against the surface, and 4.73:1 /
 * 5.61:1 on a selected row.
 */
export function sessionDotXstyle(mark: DotMark): stylex.StyleXStyles | undefined {
  return mark.fill === 'session' ? styles.levelFill : undefined;
}
