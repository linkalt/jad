/**
 * @file icons.tsx
 * @input Uses lucide-react icon components, IconRegistry type
 * @output Exports neutralIconRegistry for the neutral theme
 * @position Icon configuration for the neutral theme; consumed by index.ts
 *
 * Maps semantic icon names to Lucide icon components.
 * These icons are bundled with the theme, not with @astryxdesign/core.
 */

import React from 'react';

// The theme builder loads this file with the classic JSX transform, which needs
// React in scope; the app's tsconfig (react-jsx) would otherwise flag the import
// as unused, so the reference below keeps both compilers happy.
void React;

import type {IconRegistry, NamespacedIconName} from '@astryxdesign/core/Icon';

import {
  BookOpen,
  Brain,
  X,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  ChevronsLeft,
  ChevronsRight,
  Check,
  CheckCircle,
  XCircle,
  AlertTriangle,
  Info,
  Calendar,
  Clock,
  ExternalLink,
  Menu,
  MoreHorizontal,
  Search,
  Upload,
  ArrowUp,
  ArrowDown,
  ArrowUpDown,
  Filter,
  EyeOff,
  Columns,
  Copy,
  CheckCheck,
  Wrench,
  Square,
  Mic,
} from 'lucide-react';

const iconProps = {
  size: '1em',
  'aria-hidden': true as const,
};

// Namespaced keys sit outside Astryx's semantic set, so the registry is typed
// the way the library types its own defaults: the full built-in set plus any
// `<namespace>:<name>` extension. They keep `size`, `color` and `xstyle`.
export const neutralIconRegistry: IconRegistry &
  Partial<Record<NamespacedIconName, React.ReactNode>> = {
  close: <X {...iconProps} />,
  chevronDown: <ChevronDown {...iconProps} />,
  chevronLeft: <ChevronLeft {...iconProps} />,
  chevronRight: <ChevronRight {...iconProps} />,
  chevronsLeft: <ChevronsLeft {...iconProps} />,
  chevronsRight: <ChevronsRight {...iconProps} />,
  check: <Check {...iconProps} />,
  success: <CheckCircle {...iconProps} />,
  error: <XCircle {...iconProps} />,
  warning: <AlertTriangle {...iconProps} />,
  info: <Info {...iconProps} />,
  calendar: <Calendar {...iconProps} />,
  clock: <Clock {...iconProps} />,
  externalLink: <ExternalLink {...iconProps} />,
  menu: <Menu {...iconProps} />,
  moreHorizontal: <MoreHorizontal {...iconProps} />,
  search: <Search {...iconProps} />,
  upload: <Upload {...iconProps} />,
  arrowUp: <ArrowUp {...iconProps} />,
  arrowDown: <ArrowDown {...iconProps} />,
  arrowsUpDown: <ArrowUpDown {...iconProps} />,
  funnel: <Filter {...iconProps} />,
  eyeSlash: <EyeOff {...iconProps} />,
  viewColumns: <Columns {...iconProps} />,
  copy: <Copy {...iconProps} />,
  checkDouble: <CheckCheck {...iconProps} />,
  wrench: <Wrench {...iconProps} />,
  stop: <Square {...iconProps} />,
  microphone: <Mic {...iconProps} />,

  // App vocabulary for the composer's model picker, which has no Astryx semantic
  // name: `app:model` is the model that answers a turn.
  'app:model': <Brain {...iconProps} />,
  // The Jad usage page.
  'app:readme': <BookOpen {...iconProps} />,
};
