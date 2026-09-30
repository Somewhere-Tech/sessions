import { machineFailure, signedMachineRequest } from '../../_lib/machines';

export default async function machine(req: Request, sw: any): Promise<Response> {
  if (req.method !== 'GET' && req.method !== 'DELETE') {
    return Response.json({ error: 'method not allowed' }, { status: 405, headers: { Allow: 'GET, DELETE' } });
  }
  try {
    const verified = await signedMachineRequest(req, sw, '');
    const id = String((req as Request & { params?: { id?: string } }).params?.id || '');
    if (!id) return Response.json({ error: 'machine id is required' }, { status: 400 });
    const found = await sw.db.from('machines', { where: { id }, limit: 1 });
    if (!found.data[0]) return Response.json({ error: 'machine not found' }, { status: 404 });
    if (req.method === 'DELETE') {
      if (verified.machineID !== id) return Response.json({ error: 'a machine may remove only itself' }, { status: 403 });
      await sw.db.remove('machines', { where: { id } });
      return Response.json({ ok: true });
    }
    return Response.json({ machine: found.data[0] });
  } catch (error) {
    return machineFailure(error);
  }
}
