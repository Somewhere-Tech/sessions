import { method, requiredString } from '../../_lib/http';
import { machineFailure, signedMachineRequest } from '../../_lib/machines';

export default async function heartbeat(req: Request, sw: any): Promise<Response> {
  const refused = method(req, 'POST');
  if (refused) return refused;
  const bodyText = await req.text();
  try {
    const machineID = requiredString((JSON.parse(bodyText) as { machine_id?: unknown }).machine_id, 'machine_id');
    const verified = await signedMachineRequest(req, sw, bodyText);
    if (verified.machineID !== machineID) {
      return Response.json({ error: 'signed machine id does not match the payload' }, { status: 401 });
    }
    const limit = await sw.rateLimit.check(`heartbeat:${machineID}`, 12, 60);
    if (!limit.allowed) {
      return Response.json({ error: 'heartbeat rate limited', retry_at: limit.reset }, {
        status: 429, headers: { 'Retry-After': String(Math.max(1, limit.reset - Math.floor(Date.now() / 1000))) },
      });
    }
    const now = new Date().toISOString();
    await sw.db.update('machines', { where: { id: machineID }, set: { last_seen_at: now, updated_at: now } });
    return Response.json({ ok: true, last_seen_at: now });
  } catch (error) {
    return machineFailure(error);
  }
}
