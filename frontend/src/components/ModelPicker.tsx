import {DropdownMenu} from '@astryxdesign/core/DropdownMenu';
import {Icon} from '@astryxdesign/core/Icon';

type ModelPickerProps = {
  models: readonly string[];
  selectedModel: string | undefined;
  onSelect: (model: string) => void;
};

export function ModelPicker({models, selectedModel, onSelect}: ModelPickerProps) {
  if (models.length === 0) {
    return null;
  }

  return (
    <DropdownMenu
      button={{
        label: 'Model',
        tooltip: selectedModel === undefined ? 'Select a model' : `Current model: ${selectedModel}`,
        icon: <Icon icon="viewColumns" size="sm" />,
        variant: 'ghost',
        size: 'sm',
      }}
      hasChevron={false}
      placement="above"
      menuWidth="max-content"
      items={models.map(model => ({
        id: model,
        label: model,
        ...(model === selectedModel ? {endContent: <Icon icon="check" size="sm" />} : {}),
        onClick: () => onSelect(model),
      }))}
    />
  );
}
