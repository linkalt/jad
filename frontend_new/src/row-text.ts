import {fontWeightVars, typeScaleVars, typographyVars} from '@astryxdesign/core/theme/tokens.stylex';
import * as stylex from '@stylexjs/stylex';

const styles = stylex.create({
  // Hidden probes used only to resolve the fonts a tool row renders with.
  textProbe: {
    position: 'absolute',
    insetBlockStart: 0,
    visibility: 'hidden',
    pointerEvents: 'none',
    whiteSpace: 'nowrap',
  },
  textProbeBody: {
    fontFamily: typographyVars['--font-family-body'],
    fontSize: typeScaleVars['--text-supporting-size'],
    fontWeight: fontWeightVars['--font-weight-normal'],
  },
  textProbeCode: {
    fontFamily: typographyVars['--font-family-code'],
    fontSize: typeScaleVars['--text-supporting-size'],
    fontWeight: fontWeightVars['--font-weight-medium'],
  },
});

export function oneLine(value: string): string {
  return value.replace(/\s+/g, ' ').trim();
}

// A tool or reasoning row shares its width between the tool name and the title,
// and the row layout gives the title `flex-shrink: 10` while the tool name only
// keeps a `4ch` minimum - so any title that does not fit clips the name to
// `writ…`. Titles are therefore cut to the measured row width, which lets the
// title carry the truncation instead of the name.
const TOOL_ROW_CHROME_PX = 96;

const TOOL_TITLE_SAFETY_PX = 8;

const TOOL_TITLE_FALLBACK_PX_PER_CHAR = 7;

type RowTextFont = 'body' | 'code';

const resolvedRowFonts: Partial<Record<RowTextFont, string>> = {};

let rowTextCanvas: HTMLCanvasElement | null = null;

function resolveRowFont(font: RowTextFont): string {
  const cached = resolvedRowFonts[font];
  if (cached || typeof document === 'undefined') {
    return cached ?? `${TOOL_TITLE_FALLBACK_PX_PER_CHAR}px`;
  }
  const probe = document.createElement('span');
  probe.setAttribute('aria-hidden', 'true');
  probe.className =
    stylex.props(
      styles.textProbe,
      font === 'code' ? styles.textProbeCode : styles.textProbeBody,
    ).className ?? '';
  // Measure inside the theme, where the rows render: on `document.body` the
  // `--font-family-*` tokens only see the :root defaults, so the budget would
  // be computed from a different face than the row is drawn with.
  const probeHost =
    document.getElementById('root')?.firstElementChild ?? document.body;
  probeHost.appendChild(probe);
  const probeStyles = getComputedStyle(probe);
  const resolved = `${probeStyles.fontWeight} ${probeStyles.fontSize} ${probeStyles.fontFamily}`;
  probe.remove();
  resolvedRowFonts[font] = resolved;
  return resolved;
}

/** Width of row text in px, measured with the font the row renders it in. */
function measureRowText(text: string, font: RowTextFont): number {
  if (typeof document === 'undefined') {
    return text.length * TOOL_TITLE_FALLBACK_PX_PER_CHAR;
  }
  rowTextCanvas ??= document.createElement('canvas');
  const context = rowTextCanvas.getContext('2d');
  if (!context) {
    return text.length * TOOL_TITLE_FALLBACK_PX_PER_CHAR;
  }
  context.font = resolveRowFont(font);
  return context.measureText(text).width;
}

/** Cut text to a pixel width, ending with an ellipsis when it does not fit. */
export function cutRowText(text: string, maxPx: number): string {
  if (maxPx <= 0) {
    return '';
  }
  if (measureRowText(text, 'body') <= maxPx) {
    return text;
  }
  let fits = 0;
  let tooLong = text.length;
  while (fits < tooLong) {
    const middle = Math.ceil((fits + tooLong) / 2);
    if (measureRowText(`${text.slice(0, middle)}…`, 'body') <= maxPx) {
      fits = middle;
    } else {
      tooLong = middle - 1;
    }
  }
  return fits > 0 ? `${text.slice(0, fits)}…` : '…';
}

/**
 * Width a row title may use so its tool name keeps full width: the measured row
 * width minus the fixed row furniture (status icon, gaps, count, chevron,
 * padding), the name itself and a safety margin.
 */
export function toolTitleBudgetPx(rowWidthPx: number, toolName: string): number {
  if (rowWidthPx <= 0) {
    return 0;
  }
  return Math.max(
    0,
    rowWidthPx -
      TOOL_ROW_CHROME_PX -
      measureRowText(toolName, 'code') -
      TOOL_TITLE_SAFETY_PX,
  );
}
