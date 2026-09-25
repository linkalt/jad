import {DropdownMenu} from '@astryxdesign/core/DropdownMenu';
import {Icon} from '@astryxdesign/core/Icon';
import {composerPickerButton} from '../picker-button';

type ModelPickerProps = {
  models: readonly string[];
  selectedModel: string | undefined;
  isDisabled?: boolean;
  onSelect: (model: string) => void;
};

export function ModelPicker({models, selectedModel, isDisabled = false, onSelect}: ModelPickerProps) {
  if (models.length === 0) {
    return null;
  }

  return (
    <DropdownMenu
      button={composerPickerButton({
        label: 'Model',
        tooltip: selectedModel === undefined ? 'Select a model' : `Current model: ${selectedModel}`,
        icon: <Icon icon="app:model" size="sm" />,
        isDisabled,
      })}
      hasChevron={false}
      placement="above"
      menuWidth="max-content"
      items={models.map(model => ({
        id: model,
        label: model,
        ...(model === selectedModel ? {endContent: <Icon icon="check" size="sm" />} : {}),
        // Also disable the items: a menu left open when a run starts must not
        // change the model of the run in flight.
        isDisabled,
        onClick: () => onSelect(model),
      }))}
    />
  );
}
