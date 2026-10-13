import {DropdownMenu} from '@astryxdesign/core/DropdownMenu';
import {Icon} from '@astryxdesign/core/Icon';
import {composerPickerButton} from '../picker-button';
import {profileLabel} from '../session-views';

type SessionProfilePickerProps = {
  profiles: readonly string[];
  profileNames?: Readonly<Record<string, string>> | undefined;
  activeProfile: string | undefined;
  isDisabled?: boolean;
  onSelect: (profile: string) => void;
};

export function SessionProfilePicker({
  profiles,
  profileNames,
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
          : `Active session profile: ${profileLabel(activeProfile, profileNames)}`,
        icon: <Icon icon="app:profile" size="sm" />,
        isDisabled,
      })}
      hasChevron={false}
      placement="above"
      menuWidth="max-content"
      items={profiles.map(profile => ({
        id: profile,
        label: profileLabel(profile, profileNames),
        ...(profileNames?.[profile] === undefined ? {} : {description: profile}),
        ...(profile === activeProfile ? {endContent: <Icon icon="check" size="sm" />} : {}),
        isDisabled,
        onClick: () => onSelect(profile),
      }))}
    />
  );
}
