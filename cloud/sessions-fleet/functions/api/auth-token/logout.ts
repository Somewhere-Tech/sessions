import { authHandler } from '../../_lib/auth';

export default authHandler('POST', async (body, sw, req) => {
  await sw.auth.requireUser(req);
  await sw.auth.logout({
    session_token: typeof body.session_token === 'string' ? body.session_token : undefined,
    refresh_token: typeof body.refresh_token === 'string' ? body.refresh_token : undefined,
  });
  return { ok: true };
});
