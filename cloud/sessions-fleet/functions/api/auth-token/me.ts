import { failure, method } from '../../_lib/http';

export default async function me(req: Request, sw: any): Promise<Response> {
  const refused = method(req, 'GET');
  if (refused) return refused;
  try {
    return Response.json({ user: await sw.auth.requireUser(req) });
  } catch (error) {
    return failure(error);
  }
}
