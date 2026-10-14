import type {SVGProps} from 'react';

// The icon registry has no folder glyph; Astryx accepts a plain SVG component anywhere an icon name goes.
export function FolderIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.5} {...props}>
      <path
        d="M3.75 7.25a1.5 1.5 0 0 1 1.5-1.5h4.1l1.9 2h7.5a1.5 1.5 0 0 1 1.5 1.5v8.25a1.5 1.5 0 0 1-1.5 1.5H5.25a1.5 1.5 0 0 1-1.5-1.5z"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}
