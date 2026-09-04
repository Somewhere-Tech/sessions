import { method } from '../../_lib/http';
import { machineFailure, signedMachineRequest } from '../../_lib/machines';

export default async function index(req: Request, sw: any): Promise<Response> {
  const refused = method(req, 'GET');
  if (refused) return refused;
  try {
    await signedMachineRequest(req, sw, '');
    const machines = await sw.db.from('machines', { order: [['name', 'asc'], ['id', 'asc']], limit: 500 });
    return Response.json({ machines: machines.data });
  } catch (error) {
    return machineFailure(error);
  }
}
