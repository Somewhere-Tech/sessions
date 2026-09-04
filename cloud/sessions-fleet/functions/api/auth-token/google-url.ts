import { authHandler, requiredString } from '../../_lib/auth';

export default authHandler('POST', async (body, sw) => ({
  url: sw.auth.googleUrl({ redirect_uri: requiredString(body.redirect_uri, 'redirect_uri') }),
}));
