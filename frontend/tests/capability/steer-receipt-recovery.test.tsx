import { describe, expect, it, vi } from 'vitest';
import { submitMessage } from '../../src/api/sessionsd/operations';
import { useFakeMachines } from './fake-daemon';

function setup(): void {
  useFakeMachines([{
    id: 'local', name: 'Fixture Mac', host: 'localhost', port: 8787,
    isDefault: true, sessions: []
  }]);
}

describe('capability: steering receipts survive broken HTTP responses', () => {
  it.each(['invalid-json', 'error-body'])('recovers the same operation after %s without resending', async (failure) => {
    setup();
    let operation = '';
    const fetch = vi.fn(async (_url: unknown, init?: RequestInit) => {
      if (init?.method === 'POST') {
        operation = JSON.parse(String(init.body)).operation_id;
        return failure === 'invalid-json'
          ? new Response('{', { status: 200 })
          : Response.json({ error: 'could not record message outcome; do not automatically resend' }, { status: 500 });
      }
      return Response.json({ operation_id: operation, status: 'accepted', delivered: true, retry: false, acceptance: 'provider' });
    });
    globalThis.fetch = fetch;
    await expect(submitMessage('fixture', 'Change the answer', 'local', undefined, 'steer')).resolves.toBeUndefined();
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(String(fetch.mock.calls[1]?.[0])).toContain(`/api/message-deliveries/${operation}`);
    expect(fetch.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1);
  });

  it('reports unknown when neither the response nor receipt can confirm delivery', async () => {
    setup();
    globalThis.fetch = vi.fn(async () => new Response('{', { status: 502 }));
    await expect(submitMessage('fixture', 'Change the answer', 'local', undefined, 'steer'))
      .rejects.toMatchObject({ name: 'MessageDeliveryError', deliveryStatus: 'unknown' });
  });
});
