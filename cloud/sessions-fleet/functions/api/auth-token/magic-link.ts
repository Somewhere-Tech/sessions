import { authHandler, requiredString } from '../../_lib/auth';

export default authHandler('POST', async (body, sw) => {
  await sw.auth.signInWithOtp({ email: requiredString(body.email, 'email') });
  return { ok: true };
});
