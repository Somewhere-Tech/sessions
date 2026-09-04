import { authHandler, requiredString } from '../../_lib/auth';

export default authHandler('POST', async (body, sw) => sw.auth.login({
  email: requiredString(body.email, 'email'),
  password: requiredString(body.password, 'password'),
}));
