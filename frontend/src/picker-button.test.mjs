import test from 'node:test';
import assert from 'node:assert/strict';
import {composerPickerButton} from './picker-button.ts';

const base = {
  label: 'Model',
  tooltip: 'Current model: model.json',
  icon: '<icon/>',
  isDisabled: false,
};

test('the picker trigger renders as an icon-only ghost button', () => {
  const button = composerPickerButton(base);
  assert.equal(button.isIconOnly, true);
  assert.equal(button.label, 'Model');
  assert.equal(button.variant, 'ghost');
  assert.equal(button.size, 'sm');
});

test('the label survives as the accessible name and the tooltip carries the value', () => {
  const button = composerPickerButton(base);
  assert.equal(button.tooltip, 'Current model: model.json');
  assert.equal(button.icon, '<icon/>');
});

test('a provider run disables the trigger, and clearing it re-enables', () => {
  assert.equal(composerPickerButton({...base, isDisabled: true}).isDisabled, true);
  assert.equal(composerPickerButton({...base, isDisabled: false}).isDisabled, false);
});
