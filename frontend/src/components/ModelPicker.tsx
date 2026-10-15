import {useState} from 'react';
import {DropdownMenu} from '@astryxdesign/core/DropdownMenu';
import {Icon} from '@astryxdesign/core/Icon';
import {TextInput, VStack} from '@astryxdesign/core';
import {composerPickerButton} from '../picker-button';
import {filterModels} from '../model-selection';

type ModelPickerProps = {
  models: readonly string[];
  selectedModel: string | undefined;
  isDisabled?: boolean;
  onSelect: (model: string) => void;
};

export function ModelPicker({models, selectedModel, isDisabled = false, onSelect}: ModelPickerProps) {
  const [query, setQuery] = useState('');

  if (models.length === 0) {
    return null;
  }

  const visibleModels = filterModels(models, query);
  const trimmedQuery = query.trim();

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
      onOpenChange={isOpen => {
        if (!isOpen) setQuery('');
      }}
      header={
        // Clicks in the menu popover bubble through the React portal to the
        // chat composer's click-to-focus handler, which would pull focus back
        // into the draft input. Contain them here so the search field keeps
        // focus. Outside-click dismissal is DOM-contained, so it is unaffected.
        <VStack onClick={event => event.stopPropagation()}>
          <TextInput
            label="Search models"
            isLabelHidden
            placeholder="Search models"
            startIcon="search"
            hasClear
            size="sm"
            width="100%"
            value={query}
            onChange={setQuery}
          />
        </VStack>
      }
      items={
        visibleModels.length === 0
          ? [
              {
                id: 'no-models-match',
                label: `No models match “${trimmedQuery}”.`,
                isDisabled: true,
              },
            ]
          : visibleModels.map(model => ({
              id: model,
              label: model,
              ...(model === selectedModel ? {endContent: <Icon icon="check" size="sm" />} : {}),
              // Also disable the items: a menu left open when a run starts must not
              // change the model of the run in flight.
              isDisabled,
              onClick: () => onSelect(model),
            }))
      }
    />
  );
}
