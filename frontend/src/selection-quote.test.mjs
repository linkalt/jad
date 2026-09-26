import test from 'node:test';
import assert from 'node:assert/strict';

import {
  quoteSelection,
  quoteDraftText,
  quoteDraftForSelection,
  shouldOfferQuote,
} from './selection-quote.ts';

const assistantSelection = (text) => ({
  text,
  isInsideAssistantMessage: true,
  isInsideToolOutput: false,
});

test('a selected line becomes one quoted line', () => {
  assert.equal(quoteSelection('the diff is 65 lines', 2000)?.quote, '> the diff is 65 lines');
});

test('every line of a multi-line selection is quoted', () => {
  const quote = quoteSelection('first line\nsecond line\nthird line', 2000);
  assert.equal(quote?.quote, '> first line\n> second line\n> third line');
});

test('a blank line inside the selection keeps the marker alone, so the quote stays one block', () => {
  const quote = quoteSelection('before\n\nafter', 2000);
  assert.equal(quote?.quote, '> before\n>\n> after');
});

test('trailing spaces and runs of blank lines are not carried into the message', () => {
  const quote = quoteSelection('  spaced out   \n\n\n\nnext   ', 2000);
  assert.equal(quote?.quote, '> spaced out\n>\n> next');
});

test('carriage returns are read as line breaks', () => {
  assert.equal(quoteSelection('one\r\ntwo\rthree', 2000)?.quote, '> one\n> two\n> three');
});

test('indentation survives, because inside a quote it is often the code layout', () => {
  const quote = quoteSelection('function f() {\n  return 1;\n}', 2000);
  assert.equal(quote?.quote, '> function f() {\n>   return 1;\n> }');
});

test('a selection longer than the budget is cut where it stops and says so', () => {
  const quote = quoteSelection('x'.repeat(10), 4);
  assert.equal(quote?.quote, '> xxxx…[truncated]');
  assert.equal(quote?.isTruncated, true);
  // The count reports what was selected, not what survived the cut.
  assert.equal(quote?.characters, 10);
});

test('a selection that fits is never marked as truncated', () => {
  const quote = quoteSelection('short', 5);
  assert.equal(quote?.isTruncated, false);
  assert.equal(quote?.quote, '> short');
});

test('nothing to quote is not a quote', () => {
  assert.equal(quoteSelection('', 2000), null);
  assert.equal(quoteSelection('   \n\n  ', 2000), null);
  assert.equal(quoteSelection('text', 0), null);
});

test('the draft ends on a fresh line so the reply is not written inside the quote', () => {
  const quote = quoteSelection('quoted text', 2000);
  assert.ok(quote);
  assert.equal(quoteDraftText(quote), '> quoted text\n');
  // The composer appends the draft to whatever is already there, so the reply
  // starts on the line below the last quoted line.
  assert.equal(`${'> earlier\n'}\n${quoteDraftText(quote)}`.endsWith('> quoted text\n'), true);
});

test('only a selection inside one assistant message offers the quote action', () => {
  assert.equal(shouldOfferQuote(assistantSelection('ambiguous bit')), true);
  assert.equal(shouldOfferQuote({...assistantSelection('mine'), isInsideAssistantMessage: false}), false);
  assert.equal(shouldOfferQuote({...assistantSelection('raw tool document'), isInsideToolOutput: true}), false);
  assert.equal(shouldOfferQuote(assistantSelection('   ')), false);
});

test('the draft is built only for a selection that may be quoted', () => {
  assert.equal(quoteDraftForSelection(assistantSelection('a\nb'), 2000), '> a\n> b\n');
  assert.equal(
    quoteDraftForSelection({...assistantSelection('a'), isInsideToolOutput: true}, 2000),
    null,
  );
});
