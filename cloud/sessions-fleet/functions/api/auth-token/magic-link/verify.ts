import { authHandler, requiredString } from '../../../_lib/auth';

export default authHandler('POST', async (body, sw) => sw.auth.verifyOtp({
  token: requiredString(body.token, 'token'),
}));
