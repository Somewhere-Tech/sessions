import type { FleetDirectoryMachine } from '../api/sessionsd';
import { directoryCandidates } from './accountFleet';
import { rememberNativeMachineClaim, rememberServerEndpoint } from './hostedBootstrap';
import { randomUUID } from './uuid';
import { useServers } from './servers';
import type { NativePairingClaim } from './tauriBridge';

const FLEET_URL = 'https://sessions-fleet.somewhere.site';
const STORAGE_KEY = 'sessions:client-fleet-account';
const CLAIM_PATH = '/api/lan/access/account-claim';

interface ClientFleetState {
	version: 1;
	tokens: { access: string; refresh: string; session?: string };
	user: { id: string; email: string; display_name?: string };
	machineId: string;
	publicKey: string;
	privateKey: JsonWebKey;
}

export interface ClientFleetStatus {
	signedIn: boolean;
	user?: ClientFleetState['user'];
}

function readState(): ClientFleetState | null {
	try {
		const value = JSON.parse(window.localStorage.getItem(STORAGE_KEY) ?? 'null') as ClientFleetState | null;
		return value?.version === 1 && value.tokens?.access && value.tokens?.refresh && value.machineId ? value : null;
	} catch {
		return null;
	}
}

function writeState(state: ClientFleetState): void {
	window.localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
}

export function clientFleetStatus(): ClientFleetStatus {
	const state = readState();
	return state ? { signedIn: true, user: state.user } : { signedIn: false };
}

async function responseJSON<T>(response: Response, label: string): Promise<T> {
	const body = await response.text();
	if (!response.ok) {
		try {
			const failure = JSON.parse(body) as { error?: string };
			throw new Error(failure.error || `${label} returned HTTP ${response.status}`);
		} catch (reason) {
			if (reason instanceof Error && !reason.message.startsWith('Unexpected')) throw reason;
			throw new Error(`${label} returned HTTP ${response.status}`);
		}
	}
	try {
		return JSON.parse(body) as T;
	} catch {
		throw new Error(`${label} returned invalid JSON`);
	}
}

function applyRotation(state: ClientFleetState, headers: Headers): void {
	const access = headers.get('X-New-Access-Token');
	const refresh = headers.get('X-New-Refresh-Token');
	if (!access || !refresh) return;
	state.tokens = { ...state.tokens, access, refresh };
	writeState(state);
}

async function basicFleetRequest<T>(path: string, body: unknown): Promise<T> {
	const response = await fetch(FLEET_URL + path, {
		method: 'POST', redirect: 'error',
		headers: { 'content-type': 'application/json', accept: 'application/json' },
		body: JSON.stringify(body)
	});
	return responseJSON<T>(response, 'Somewhere sign-in');
}

export async function requestClientFleetMagicLink(email: string): Promise<void> {
	await basicFleetRequest('/api/auth-token/magic-link', { email: email.trim() });
}

export async function verifyClientFleetMagicLink(token: string): Promise<ClientFleetStatus> {
	const result = await basicFleetRequest<{
		user: ClientFleetState['user']; token: string; refresh_token: string; session_token?: string;
	}>('/api/auth-token/magic-link/verify', { token: token.trim() });
	if (!result.token || !result.refresh_token || !result.user?.id) {
		throw new Error('Somewhere sign-in returned an invalid token pair');
	}
	const keys = await createClientKeys();
	const state: ClientFleetState = {
		version: 1, tokens: { access: result.token, refresh: result.refresh_token, session: result.session_token },
		user: result.user, machineId: randomUUID(), publicKey: keys.publicKey, privateKey: keys.privateKey
	};
	writeState(state);
	try {
		await registerClientDevice(state);
	} catch (error) {
		window.localStorage.removeItem(STORAGE_KEY);
		throw error;
	}
	return { signedIn: true, user: state.user };
}

async function createClientKeys(): Promise<{ publicKey: string; privateKey: JsonWebKey }> {
	const pair = await crypto.subtle.generateKey({ name: 'Ed25519' }, true, ['sign', 'verify']) as CryptoKeyPair;
	const [publicBytes, privateKey] = await Promise.all([
		crypto.subtle.exportKey('raw', pair.publicKey),
		crypto.subtle.exportKey('jwk', pair.privateKey)
	]);
	return { publicKey: base64URL(new Uint8Array(publicBytes)), privateKey };
}

function clientName(): string {
	if (/iPhone|iPad|iPod/i.test(navigator.userAgent)) return 'Sessions on iPhone';
	if (/Android/i.test(navigator.userAgent)) return 'Sessions on Android';
	return 'Sessions client';
}

async function registerClientDevice(state: ClientFleetState): Promise<void> {
	await signedFleetRequest('/api/machines/register', 'POST', {
		machine_id: state.machineId,
		name: clientName(),
		machine_public_key: state.publicKey,
		endpoints_json: {},
		daemon_version: 'mobile-client'
	}, state);
}

async function signedFleetRequest<T>(path: string, method: string, value: unknown, supplied?: ClientFleetState): Promise<T> {
	const state = supplied ?? readState();
	if (!state) throw new Error('Sign in to Somewhere first');
	const body = value === undefined ? '' : JSON.stringify(value);
	const timestamp = Math.floor(Date.now() / 1000).toString();
	const nonce = randomNonce();
	const hash = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(body));
	const canonical = state.machineId + timestamp + nonce + method + path + hex(hash);
	const signature = await signText(state.privateKey, canonical);
	const response = await fetch(FLEET_URL + path, {
		method, redirect: 'error', body: body || undefined,
		headers: {
			accept: 'application/json', ...(body ? { 'content-type': 'application/json' } : {}),
			authorization: `Bearer ${state.tokens.access}`, 'X-Refresh-Token': state.tokens.refresh,
			'X-Sessions-Machine-ID': state.machineId, 'X-Sessions-Timestamp': timestamp,
			'X-Sessions-Nonce': nonce, 'X-Sessions-Signature': signature
		}
	});
	applyRotation(state, response.headers);
	return responseJSON<T>(response, 'Somewhere fleet directory');
}

async function signText(privateKey: JsonWebKey, value: string): Promise<string> {
	const key = await crypto.subtle.importKey('jwk', privateKey, { name: 'Ed25519' }, false, ['sign']);
	const signature = await crypto.subtle.sign({ name: 'Ed25519' }, key, new TextEncoder().encode(value));
	return base64URL(new Uint8Array(signature));
}

function randomNonce(): string {
	const bytes = new Uint8Array(18);
	crypto.getRandomValues(bytes);
	return base64URL(bytes);
}

function base64URL(bytes: Uint8Array): string {
	let binary = '';
	for (const byte of bytes) binary += String.fromCharCode(byte);
	return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function hex(value: ArrayBuffer): string {
	return Array.from(new Uint8Array(value), (byte) => byte.toString(16).padStart(2, '0')).join('');
}

async function clientDirectory(): Promise<FleetDirectoryMachine[]> {
	const result = await signedFleetRequest<{ machines: FleetDirectoryMachine[] }>('/api/machines/index', 'GET', undefined);
	return Array.isArray(result.machines) ? result.machines : [];
}

async function accountClaim(machineId: string, state: ClientFleetState): Promise<Record<string, string>> {
	const unsigned = {
		machine_id: machineId, device_id: state.machineId,
		timestamp: Math.floor(Date.now() / 1000).toString(), nonce: randomNonce()
	};
	const body = JSON.stringify(unsigned);
	const hash = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(body));
	const canonical = unsigned.machine_id + unsigned.device_id + unsigned.timestamp + unsigned.nonce +
		'POST' + CLAIM_PATH + hex(hash);
	return { ...unsigned, signature: await signText(state.privateKey, canonical) };
}

async function claimClientMachine(machine: FleetDirectoryMachine, state: ClientFleetState): Promise<NativePairingClaim> {
	const claim = await accountClaim(machine.id, state);
	let lastError: unknown = new Error('machine has no direct transport');
	for (const candidate of directoryCandidates(machine).filter((item) => item.transport !== 'relay')) {
		try {
			const response = await fetch(candidate.endpoint + CLAIM_PATH, {
				method: 'POST', redirect: 'error',
				headers: { 'content-type': 'application/json', accept: 'application/json' },
				body: JSON.stringify(claim)
			});
			const issued = await responseJSON<Record<string, string>>(response, `Account access to ${machine.name}`);
			const verified = await fetch(candidate.endpoint + '/api/sessions', {
				redirect: 'error', headers: { authorization: `Bearer ${issued.token}` }
			});
			verified.body?.cancel();
			if (!verified.ok || issued.machine_id !== machine.id) {
				throw new Error(`The credential issued by ${machine.name} could not be verified`);
			}
			return {
				endpoint: candidate.endpoint, machineId: issued.machine_id, machineName: issued.machine_name,
				deviceId: issued.device_id, token: issued.token, name: issued.name,
				lanEndpoint: issued.lan_endpoint, tailnetEndpoint: issued.tailnet_endpoint,
				tailnetIpEndpoint: issued.tailnet_ip_endpoint
			};
		} catch (reason) {
			lastError = reason;
		}
	}
	throw lastError;
}

export async function syncClientAccountFleet(): Promise<string[]> {
	const state = readState();
	if (!state) return [];
	const errors: string[] = [];
	for (const machine of await clientDirectory()) {
		if (!machine.id || machine.id === state.machineId || directoryCandidates(machine).length === 0) continue;
		const existing = useServers.getState().servers.find((server) => server.machineId === machine.id);
		if (existing?.token) {
			await useServers.getState().updateServer(existing.id, {
				transportCandidates: directoryCandidates(machine),
				sources: Array.from(new Set([...(existing.sources ?? ['saved']), 'account'])),
				directoryOnly: false
			});
			continue;
		}
		if (!existing?.token) {
			try {
				const server = await rememberNativeMachineClaim(await claimClientMachine(machine, state), { select: false });
				await useServers.getState().updateServer(server.id, {
					transportCandidates: directoryCandidates(machine), sources: ['account', 'saved'], directoryOnly: false
				});
				continue;
			} catch (reason) {
				errors.push(`${machine.name}: ${reason instanceof Error ? reason.message : String(reason)}`);
			}
		}
		const direct = directoryCandidates(machine).find((candidate) => candidate.transport !== 'relay');
		if (direct && direct.transport !== 'relay') {
			await rememberServerEndpoint(direct.endpoint, {
				machineId: machine.id, systemName: machine.name, select: false,
				allowPrivateHTTP: direct.transport === 'lan' || direct.transport === 'tailnet-ip',
				transport: direct.transport, transportCandidates: directoryCandidates(machine),
				sources: ['account'], directoryOnly: true,
				lanEndpoint: machine.endpoints_json.lan, tailnetEndpoint: machine.endpoints_json.tailnet,
				tailnetIpEndpoint: machine.endpoints_json.tailnet_ip
			});
		}
	}
	return errors;
}

export async function logoutClientFleetAccount(): Promise<void> {
	const state = readState();
	if (!state) return;
	await signedFleetRequest(`/api/machines/${encodeURIComponent(state.machineId)}`, 'DELETE', undefined, state);
	await signedFleetRequest('/api/auth-token/logout', 'POST', { session_token: state.tokens.session ?? '' }, state);
	window.localStorage.removeItem(STORAGE_KEY);
}
