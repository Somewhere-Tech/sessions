import { failure, jsonBody, method, requiredString } from './http';

export function authHandler(
  expectedMethod: string,
  operation: (body: Record<string, unknown>, sw: any, req: Request) => Promise<unknown>,
) {
  return async (req: Request, sw: any): Promise<Response> => {
    const refused = method(req, expectedMethod);
    if (refused) return refused;
    try {
      const body = await jsonBody<Record<string, unknown>>(req);
      return Response.json(await operation(body, sw, req));
    } catch (error) {
      return failure(error);
    }
  };
}

export { requiredString };
