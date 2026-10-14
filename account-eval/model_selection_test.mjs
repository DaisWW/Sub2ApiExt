import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const source = readFileSync(new URL('./web/app.js', import.meta.url), 'utf8');
const start = source.indexOf('function selectedAccounts()');
const end = source.indexOf('function renderModels()');
assert.ok(start >= 0 && end > start, 'Model selection functions must be present');
function selection(state = { accounts: [], groups: [], selected: new Set() }) {
  const context = vm.createContext({ state });
  vm.runInContext(source.slice(start, end), context);
  return context;
}

for (const [name, models, expected] of [
  ['latest channel text model', ['gpt-5.6-terra', 'gpt-6-sol', 'gpt-6.1-sol', 'gpt-image-99'], 'gpt-6.1-sol'],
  ['numeric minor versions', ['gpt-6.2-sol', 'gpt-6.10-sol'], 'gpt-6.10-sol'],
  ['legacy date suffix', ['gpt-4-0613', 'gpt-4.1'], 'gpt-4.1'],
  ['context window suffix', ['gpt-4-32k', 'gpt-4-32k-0613', 'gpt-4.1'], 'gpt-4.1'],
  ['dated snapshots', ['gpt-5-20250807', 'gpt-5-2025-08-07', 'gpt-5.1'], 'gpt-5.1'],
  ['hyphenated Claude versions', ['claude-3-5-sonnet-20241022', 'claude-sonnet-4-5', 'claude-opus-4-1'], 'claude-sonnet-4-5'],
  ['specialized models excluded', ['gpt-6.1-sol', 'gpt-99-audio-preview', 'gpt-99-realtime', 'text-embedding-99', 'tts-99', 'dall-e-99'], 'gpt-6.1-sol'],
  ['image generation excluded', ['gemini-3-pro-image-preview', 'gemini-2.5-pro'], 'gemini-2.5-pro'],
  ['no text models', ['gpt-image-2', 'gpt-audio-3'], ''],
  ['empty catalog', [], '']
]) {
  test(name, () => assert.equal(selection().latestTextModel(models), expected));
}
