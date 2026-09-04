import { authHandler, requiredString } from '../../_lib/auth';

export default authHandler('POST', async (body, sw) => sw.auth.googleExchange({
  code: requiredString(body.code, 'code'),
}));
