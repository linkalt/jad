import {useCallback, useState} from 'react';
import {Button, HStack, List, ListItem, Text, TextInput, Token, VStack} from '@astryxdesign/core';
import {TextArea} from '@astryxdesign/core/TextArea';
import {Selector} from '@astryxdesign/core/Selector';
import type {ResizableRegion} from '@astryxdesign/core/Resizable';
import type {PromptCategory, PromptTemplate} from '../promptLibrary';
import {
  createPromptId,
  loadPrompts,
  PROMPT_CATEGORIES,
  savePrompts,
  seedLibrary,
} from '../promptStore';
import {FloatingPanel} from './FloatingPanel';

type PromptLibraryPanelProps = {
  region: ResizableRegion;
  onClose: () => void;
  /** Called with the chosen prompt when a row is used. */
  onUsePrompt: (prompt: PromptTemplate) => void;
};

type PanelMode = 'browse' | 'manage';

/**
 * First occupant of the shared floating panel shell, now with an owner.
 *
 * Prompts are user content: they are stored through `promptStore` (localStorage)
 * rather than shipped as constants, and the panel can add, edit, delete and
 * restore them. Two modes keep the row contract clean — in `browse` a row's
 * click loads the prompt into the composer; in `manage` the rows are inert and
 * carry their own Edit/Delete buttons, so no interactive control ever nests
 * inside the list item's own clickable surface.
 */
export function PromptLibraryPanel({
  region,
  onClose,
  onUsePrompt,
}: PromptLibraryPanelProps) {
  const [prompts, setPrompts] = useState<PromptTemplate[]>(loadPrompts);
  const [mode, setMode] = useState<PanelMode>('browse');
  // A draft with an empty id is a new prompt; otherwise it edits that row.
  const [draft, setDraft] = useState<PromptTemplate | null>(null);

  const commit = useCallback((next: PromptTemplate[]) => {
    setPrompts(next);
    savePrompts(next);
  }, []);

  const isValidDraft =
    draft !== null && draft.title.trim().length > 0 && draft.body.trim().length > 0;

  const saveDraft = useCallback(() => {
    if (draft === null) {
      return;
    }
    const title = draft.title.trim();
    const body = draft.body.trim();
    if (title.length === 0 || body.length === 0) {
      return;
    }
    const next =
      draft.id.length > 0
        ? prompts.map(prompt =>
            prompt.id === draft.id ? {...prompt, title, body, category: draft.category} : prompt,
          )
        : [
            ...prompts,
            {id: createPromptId(title, prompts), title, category: draft.category, body},
          ];
    commit(next);
    setDraft(null);
    setMode('browse');
  }, [commit, draft, prompts]);

  const deletePrompt = useCallback(
    (id: string) => {
      commit(prompts.filter(prompt => prompt.id !== id));
    },
    [commit, prompts],
  );

  const subtitle =
    draft !== null
      ? draft.id.length > 0
        ? 'Editing prompt'
        : 'New prompt'
      : `${prompts.length} ${prompts.length === 1 ? 'reusable prompt' : 'reusable prompts'}`;

  return (
    <FloatingPanel
      side="start"
      label="Prompt library"
      title="Prompt library"
      subtitle={subtitle}
      region={region}
      onClose={onClose}
    >
      {draft !== null ? (
        <VStack gap={4}>
          <TextInput
            label="Title"
            placeholder="What this prompt does"
            value={draft.title}
            onChange={title => setDraft(current => (current ? {...current, title} : current))}
          />
          <Selector
            label="Category"
            options={[...PROMPT_CATEGORIES]}
            value={draft.category}
            onChange={value =>
              setDraft(current =>
                current ? {...current, category: value as PromptCategory} : current,
              )
            }
          />
          <TextArea
            label="Prompt"
            rows={8}
            placeholder="The text that lands in the composer"
            value={draft.body}
            onChange={body => setDraft(current => (current ? {...current, body} : current))}
          />
          <HStack align="center" gap={2}>
            <Button
              label="Save prompt"
              variant="primary"
              size="sm"
              isDisabled={!isValidDraft}
              onClick={saveDraft}
            />
            <Button
              label="Cancel"
              variant="ghost"
              size="sm"
              onClick={() => setDraft(null)}
            />
            {draft.id.length > 0 ? (
              <Button
                label="Delete"
                variant="ghost"
                size="sm"
                onClick={() => {
                  deletePrompt(draft.id);
                  setDraft(null);
                }}
              />
            ) : null}
          </HStack>
        </VStack>
      ) : (
        <VStack gap={4}>
          <HStack align="center" justify="between" gap={2}>
            <HStack align="center" gap={1}>
              <Button
                label="New prompt"
                variant="secondary"
                size="sm"
                onClick={() =>
                  setDraft({id: '', title: '', category: 'Writing', body: ''})
                }
              />
              <Button
                label={mode === 'manage' ? 'Done' : 'Manage'}
                variant="ghost"
                size="sm"
                onClick={() => setMode(current => (current === 'manage' ? 'browse' : 'manage'))}
              />
            </HStack>
            {mode === 'manage' ? (
              <Button
                label="Restore defaults"
                variant="ghost"
                size="sm"
                onClick={() => commit(seedLibrary())}
              />
            ) : null}
          </HStack>
          {prompts.length === 0 ? (
            <Text type="supporting" color="secondary">
              No prompts saved. Add one with “New prompt”, or bring the starter set back
              with “Manage” → “Restore defaults”.
            </Text>
          ) : (
            <List
              hasDividers
              header={
                <Text type="supporting" color="secondary">
                  {mode === 'manage'
                    ? 'Edit or delete a prompt.'
                    : 'Select a prompt to load it into the composer.'}
                </Text>
              }
            >
              {prompts.map(prompt => (
                <ListItem
                  key={prompt.id}
                  label={prompt.title}
                  description={
                    <Text type="supporting" color="secondary">
                      {prompt.body}
                    </Text>
                  }
                  endContent={
                    mode === 'manage' ? (
                      <HStack align="center" gap={1}>
                        <Button
                          label="Edit"
                          variant="ghost"
                          size="sm"
                          onClick={() => setDraft({...prompt})}
                        />
                        <Button
                          label="Delete"
                          variant="ghost"
                          size="sm"
                          onClick={() => deletePrompt(prompt.id)}
                        />
                      </HStack>
                    ) : (
                      <Token label={prompt.category} size="sm" />
                    )
                  }
                  onClick={mode === 'manage' ? undefined : () => onUsePrompt(prompt)}
                />
              ))}
            </List>
          )}
        </VStack>
      )}
    </FloatingPanel>
  );
}
