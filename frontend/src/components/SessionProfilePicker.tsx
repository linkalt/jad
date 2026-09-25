import {DropdownMenu} from '@astryxdesign/core/DropdownMenu';
import {Icon} from '@astryxdesign/core/Icon';
import {composerPickerButton} from '../picker-button';

type SessionProfilePickerProps = {
  profiles: readonly string[];
  activeProfile: string | undefined;
  isDisabled?: boolean;
  onSelect: (profile: string) => void;
};

export function SessionProfilePicker({
  profiles,
  activeProfile,
  isDisabled = false,
  onSelect,
}: SessionProfilePickerProps) {
  if (profiles.length === 0) {
    return null;
  }

  return (
    <DropdownMenu
      button={composerPickerButton({
        label: 'Profile',
        tooltip: activeProfile === undefined
          ? 'Select a session profile'
          : `Active session profile: ${activeProfile}`,
        icon: <Icon icon="app:profile" size="sm" />,
        isDisabled,
      })}
      hasChevron={false}
      placement="above"
      menuWidth="max-content"
      items={profiles.map(profile => ({
        id: profile,
        label: profile,
        ...(profile === activeProfile ? {endContent: <Icon icon="check" size="sm" />} : {}),
        isDisabled,
        onClick: () => onSelect(profile),
      }))}
    />
  );
}
