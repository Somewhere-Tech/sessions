export function method(req: Request, expected: string): Response | null {
  if (req.method === expected) return null;
  return Response.json({ error: 'method not allowed' }, { status: 405, headers: { Allow: expected } });
}

export async function jsonBody<T>(req: Request): Promise<T> {
  const text = await req.text();
  if (!text.trim()) return {} as T;
  return JSON.parse(text) as T;
}

export function failure(error: unknown): Response {
  const value = error as { message?: string; status?: number; code?: string };
  const status = Number.isInteger(value?.status) && Number(value.status) >= 400
    ? Number(value.status)
    : 400;
  return Response.json({ error: value?.message || 'request failed', code: value?.code }, { status });
}

export function requiredString(value: unknown, name: string): string {
  if (typeof value !== 'string' || !value.trim()) {
    throw Object.assign(new Error(`${name} is required`), { status: 400, code: 'VALIDATION_ERROR' });
  }
  return value.trim();
}
