import { method, requiredString } from '../../_lib/http';
import { machineFailure, signedMachineRequest } from '../../_lib/machines';

interface RegistrationBody {
  machine_id?: unknown;
  name?: unknown;
  machine_public_key?: unknown;
  endpoints_json?: unknown;
  daemon_version?: unknown;
}

export default async function register(req: Request, sw: any): Promise<Response> {
  const refused = method(req, 'POST');
  if (refused) return refused;
  const bodyText = await req.text();
  try {
    const body = JSON.parse(bodyText) as RegistrationBody;
    const machineID = requiredString(body.machine_id, 'machine_id');
    const publicKey = requiredString(body.machine_public_key, 'machine_public_key');
    const verified = await signedMachineRequest(req, sw, bodyText, publicKey);
    if (verified.machineID !== machineID) {
      throw Object.assign(new Error('signed machine id does not match the payload'), { status: 401, code: 'MACHINE_ID_MISMATCH' });
    }
    const now = new Date().toISOString();
    const values = {
      id: machineID, name: requiredString(body.name, 'name'), machine_public_key: publicKey,
      endpoints_json: body.endpoints_json ?? {}, daemon_version: requiredString(body.daemon_version, 'daemon_version'),
      last_seen_at: now, updated_at: now,
    };
    const result = verified.machine
      ? await sw.db.update('machines', { where: { id: machineID }, set: values })
      : await sw.db.insert('machines', values);
    return Response.json({ machine: result.data[0] ?? values });
  } catch (error) {
    return machineFailure(error);
  }
}
