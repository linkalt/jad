import type {SVGProps} from 'react';

// The icon registry has no sun or moon; Astryx accepts a plain SVG component anywhere an icon name goes.
function ThemeSvg(props: SVGProps<SVGSVGElement>) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.75}
      strokeLinecap="round"
      strokeLinejoin="round"
      {...props}
    />
  );
}

export function SunIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <ThemeSvg {...props}>
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41" />
    </ThemeSvg>
  );
}

export function MoonIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <ThemeSvg {...props}>
      <path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z" />
    </ThemeSvg>
  );
}
