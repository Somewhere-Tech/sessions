import { useDurableDraft } from '../../src/hooks/useDurableDraft';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { RestartConversation, RestartConversationHost } from '../../src/components/RestartConversation';
import { makeSession } from './fake-daemon';
import { getActiveServer, useServers } from '../../src/lib/servers';
import { draftStorageKey, readDraft, saveDraft } from '../../src/lib/draftStore';

const source = makeSession({ id: 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee', name: 'Restart fixture', tool: 'claude-code', kind: 'claude-structured', model: 'fixture-model', profile: 'fixture-work', permissions: 'constrained', working: true });
const laneId = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';

function FixtureDraft(): JSX.Element {
  const draft = useDurableDraft('fixture', source.id);
  return <textarea aria-label="Fixture draft" value={draft.text} onChange={(event) => draft.setText(event.target.value)} />;
}

describe('confirmed conversation restart', () => {
  beforeEach(() => useServers.setState({ servers: [{ id: 'fixture', name: 'Fixture', host: '127.0.0.1', port: 8899, isDefault: true }], activeId: 'fixture' }));
  it('shows the exact runtime, selects YOLO, reopens automatically and retains the draft', async () => {
    const user = userEvent.setup(); const onOpen = vi.fn(); const posted: unknown[] = [];
    vi.stubGlobal('fetch', vi.fn(async (_url, options) => {
      posted.push(JSON.parse(options.body));
      return new Response(JSON.stringify({ ok: true, sourceEnded: true, sourceSessionId: source.id, operationId: 'operation', laneId }), { status: 200 });
    }));
    const machine = getActiveServer().id;
    saveDraft(draftStorageKey(machine, source.id), 'My exact unsent draft');
    render(<><RestartConversation session={source} onOpen={onOpen} /><RestartConversationHost /></>);
    await user.click(screen.getByRole('button', { name: 'Restart / change permissions…' }));
    await screen.findByRole('dialog');
    expect(screen.getByRole('dialog')).toHaveTextContent(source.id);
    expect(screen.getByRole('dialog')).toHaveTextContent('fixture-work');
    expect(screen.getByRole('dialog')).toHaveTextContent('fixture-model');
    expect(screen.getByRole('dialog')).toHaveTextContent('Restart interrupts its current turn');
    expect(posted).toHaveLength(0);
    await user.selectOptions(screen.getByRole('combobox', { name: 'Restart permissions' }), 'full');
    await user.click(screen.getByRole('checkbox'));
    await user.click(screen.getByRole('button', { name: 'End this runtime and reopen' }));
    await waitFor(() => expect(onOpen).toHaveBeenCalledExactlyOnceWith(laneId));
    expect(posted).toEqual([{ sourceSessionId: source.id, confirmSessionId: source.id, permissions: 'full', remoteControl: true, runtimeMode: 'terminal' }]);
    expect(readDraft(draftStorageKey(machine, laneId)).text).toBe('My exact unsent draft');
    expect(readDraft(draftStorageKey(machine, source.id)).text).toBe('My exact unsent draft');
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('keeps progress mounted when the source ends, locks choices and retries the same operation', async () => {
    const user = userEvent.setup(); const onOpen = vi.fn(); const posted: unknown[] = [];
    vi.stubGlobal('fetch', vi.fn(async (_url, options) => {
      posted.push(JSON.parse(options.body)); const partial = posted.length === 1;
      return new Response(JSON.stringify({ ok: !partial, partial, sourceEnded: true, sourceSessionId: source.id, operationId: 'operation', laneId,
        adoption: partial ? { warning: 'Replacement is running; its history link needs repair.' } : undefined }), { status: partial ? 202 : 200 });
    }));
    const { rerender } = render(<><RestartConversation session={source} onOpen={onOpen} /><RestartConversationHost /></>);
    await user.click(screen.getByRole('button', { name: 'Restart / change permissions…' }));
    await screen.findByRole('dialog');
    await user.click(screen.getByRole('button', { name: 'End this runtime and reopen' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('history link needs repair');
    rerender(<><RestartConversation session={{ ...source, exited: true }} onOpen={onOpen} /><RestartConversationHost /></>);
    expect(screen.getByRole('dialog')).toHaveTextContent(`Replacement: ${laneId}`);
    expect(screen.getByRole('combobox', { name: 'Restart permissions' })).toBeDisabled();
    expect(onOpen).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Retry same restart' }));
    await waitFor(() => expect(onOpen).toHaveBeenCalledWith(laneId));
    expect(posted[0]).toEqual(posted[1]);
  });
  it('keeps the submitted choices after a lost response and retries without changing them', async () => {
    const user = userEvent.setup(); const onOpen = vi.fn(); const posted: unknown[] = [];
    vi.stubGlobal('fetch', vi.fn(async (_url, options) => {
      posted.push(JSON.parse(options.body));
      if (posted.length === 1) throw new TypeError('Fixture response was lost');
      return new Response(JSON.stringify({ ok: true, sourceEnded: true, sourceSessionId: source.id, operationId: 'operation', laneId }), { status: 200 });
    }));
    render(<><RestartConversation session={source} onOpen={onOpen} /><RestartConversationHost /></>);
    await user.click(screen.getByRole('button', { name: 'Restart / change permissions…' }));
    await screen.findByRole('dialog');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Restart permissions' }), 'full');
    await user.click(screen.getByRole('button', { name: 'End this runtime and reopen' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('response was lost');
    expect(screen.getByRole('combobox', { name: 'Restart permissions' })).toBeDisabled();
    expect(onOpen).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Retry same restart' }));
    await waitFor(() => expect(onOpen).toHaveBeenCalledWith(laneId));
    expect(posted[0]).toEqual(posted[1]);
  });

  it('refuses to end the runtime when its latest unsent draft cannot be saved', async () => {
    const user = userEvent.setup(); const onOpen = vi.fn(); const fetch = vi.fn();
    vi.stubGlobal('fetch', fetch);
    render(<><FixtureDraft /><RestartConversation session={source} onOpen={onOpen} /><RestartConversationHost /></>);
    await user.type(screen.getByRole('textbox', { name: 'Fixture draft' }), 'Keep my newest edits');
    const storage = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('Fixture storage full'); });
    await user.click(screen.getByRole('button', { name: 'Restart / change permissions…' }));
    await screen.findByRole('dialog');
    await user.click(screen.getByRole('button', { name: 'End this runtime and reopen' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('original runtime is still running');
    expect(fetch).not.toHaveBeenCalled();
    expect(screen.getByRole('textbox', { name: 'Fixture draft' })).toHaveValue('Keep my newest edits');
    expect(onOpen).not.toHaveBeenCalled();
    storage.mockRestore();
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
  });

});
