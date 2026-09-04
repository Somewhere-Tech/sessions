import { authHandler, requiredString } from '../../_lib/auth';

export default authHandler('POST', async (body, sw) => sw.auth.signup({
  email: requiredString(body.email, 'email'),
  password: requiredString(body.password, 'password'),
  display_name: typeof body.display_name === 'string' ? body.display_name.trim() : undefined,
}));
