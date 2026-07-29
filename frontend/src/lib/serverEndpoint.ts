export type ServerScheme = 'http' | 'https';

export interface ParsedServerEndpoint {
  scheme: ServerScheme;
  host: string;
  port: number;
}

function isIpv4(host: string): boolean {
  const parts = host.split('.');
  return parts.length === 4 && parts.every((part) => {
    if (!/^\d{1,3}$/.test(part)) return false;
    const value = Number(part);
    return value >= 0 && value <= 255;
  });
}

function isIpv6(host: string): boolean {
  return host.startsWith('[') && host.endsWith(']') && host.includes(':');
}

export function isIpHost(host: string): boolean {
  return isIpv4(host) || isIpv6(host);
}

export function isLoopbackHost(host: string): boolean {
  const normalized = host.toLowerCase();
  return normalized === 'localhost'
    || normalized.endsWith('.localhost')
    || normalized === '[::1]'
    || normalized === '::1'
    || (isIpv4(normalized) && normalized.startsWith('127.'));
}

export function isPrivateNetworkHost(host: string): boolean {
  const normalized = host.toLowerCase();
  if (isLoopbackHost(normalized)) return true;
  if (isIpv4(normalized)) {
    const [first, second] = normalized.split('.').map(Number);
    return first === 10
      || (first === 172 && second >= 16 && second <= 31)
      || (first === 192 && second === 168)
      || (first === 169 && second === 254);
  }
  if (!isIpv6(normalized)) return false;
  const unbracketed = normalized.replace(/^\[|\]$/g, '');
  return unbracketed.startsWith('fc')
    || unbracketed.startsWith('fd')
    || /^fe[89ab]/.test(unbracketed);
}

function defaultPort(scheme: ServerScheme, hadExplicitScheme: boolean, host: string): number {
  if (!hadExplicitScheme && (isLoopbackHost(host) || isIpHost(host))) return 8787;
  return scheme === 'https' ? 443 : 80;
}

export function parseServerEndpoint(value: string): ParsedServerEndpoint {
  const input = value.trim();
  if (!input) throw new Error('Enter an endpoint.');

  const schemeMatch = input.match(/^([a-z][a-z\d+.-]*):\/\//i);
  const hadExplicitScheme = schemeMatch !== null;
  if (schemeMatch && !/^https?$/i.test(schemeMatch[1])) {
    throw new Error('Endpoint must use HTTP or HTTPS.');
  }

  let parsed: URL;
  try {
    parsed = new URL(hadExplicitScheme ? input : `http://${input}`);
  } catch {
    throw new Error('Enter a valid endpoint, such as https://mac.example.com.');
  }

  if (!parsed.hostname) throw new Error('Endpoint must include a host.');
  if (parsed.username || parsed.password) throw new Error('Credentials belong in the advanced token field.');
  if (parsed.pathname !== '/' || parsed.search || parsed.hash) {
    throw new Error('Endpoint must not include a path, query, or fragment.');
  }

  const host = parsed.hostname.toLowerCase();
  const scheme: ServerScheme = hadExplicitScheme
    ? (parsed.protocol === 'https:' ? 'https' : 'http')
    : (isLoopbackHost(host) || isIpHost(host) ? 'http' : 'https');
  const port = parsed.port ? Number(parsed.port) : defaultPort(scheme, hadExplicitScheme, host);

  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error('Port must be between 1 and 65535.');
  }

  return { scheme, host, port };
}

export function formatServerEndpoint(
  endpoint: Pick<ParsedServerEndpoint, 'host' | 'port'> & { scheme?: ServerScheme }
): string {
  return `${endpoint.scheme ?? 'http'}://${endpoint.host}:${endpoint.port}`;
}
