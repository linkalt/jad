import type {ReactNode} from 'react';
import type {DropdownMenuButtonProps} from '@astryxdesign/core/DropdownMenu';

export type ComposerPickerButtonOptions = {
  /** Accessible name. An icon-only trigger renders no visible text, so this is its label. */
  label: string;
  /** Hover text. It carries the current value, since the trigger shows no text. */
  tooltip: string;
  icon: ReactNode;
  /** A provider run, or another in-flight picker update, blocks changes until it completes. */
  isDisabled: boolean;
};

/**
 * Trigger for the composer's dropdown pickers. Every control in the composer
 * row is a square icon button, so the trigger shape lives here rather than
 * being repeated per picker: the label stays as the accessible name and the
 * disabled state always reaches the button itself (the menu items are only
 * reachable once the button opens the menu).
 */
export function composerPickerButton({
  label,
  tooltip,
  icon,
  isDisabled,
}: ComposerPickerButtonOptions): DropdownMenuButtonProps {
  return {
    label,
    tooltip,
    icon,
    variant: 'ghost',
    size: 'sm',
    isIconOnly: true,
    isDisabled,
  };
}
