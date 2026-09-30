import {
	claimFleetDirectoryMachine,
	fetchFleetDirectory,
	type FleetDirectoryMachine
} from '../api/sessionsd';
import { rememberNativeMachineClaim, rememberServerEndpoint } from './hostedBootstrap';
import { isPrivateNetworkHost } from './serverEndpoint';
import { useServers, type ServerConfig } from './servers';
import type { NativePairingClaim } from './tauriBridge';

type TransportCandidate = NonNullable<ServerConfig['transportCandidates']>[number];

export function directoryCandidates(machine: FleetDirectoryMachine): TransportCandidate[] {
	const endpoints = machine.endpoints_json ?? {};
	const values: TransportCandidate[] = [
		{ endpoint: endpoints.lan ?? '', transport: 'lan' },
		{ endpoint: endpoints.tailnet ?? '', transport: 'tailnet' },
		{ endpoint: endpoints.tailnet_ip ?? '', transport: 'tailnet-ip' },
		{ endpoint: endpoints.relay ?? '', transport: 'relay' }
	];
	const seen = new Set<string>();
	return values.filter((candidate) => {
		candidate.endpoint = candidate.endpoint.trim().replace(/\/$/, '');
		if (!candidate.endpoint || seen.has(candidate.endpoint) || !validDirectoryCandidate(candidate)) return false;
		seen.add(candidate.endpoint);
		return true;
	});
}

function validDirectoryCandidate(candidate: TransportCandidate): boolean {
	try {
		const parsed = new URL(candidate.endpoint);
		if (parsed.username || parsed.password || parsed.search || parsed.hash) return false;
		if (candidate.transport === 'relay') return parsed.protocol === 'https:';
		if (parsed.pathname !== '/') return false;
		if (candidate.transport === 'tailnet') {
			return parsed.protocol === 'https:' && parsed.hostname.toLowerCase().endsWith('.ts.net');
		}
		return parsed.protocol === 'http:' && isPrivateNetworkHost(parsed.hostname);
	} catch {
		return false;
	}
}

function nativeClaim(response: Awaited<ReturnType<typeof claimFleetDirectoryMachine>>): NativePairingClaim {
	return {
		endpoint: response.endpoint,
		machineId: response.claim.machine_id,
		machineName: response.claim.machine_name,
		deviceId: response.claim.device_id,
		token: response.claim.token,
		name: response.claim.name,
		lanEndpoint: response.claim.lan_endpoint,
		tailnetEndpoint: response.claim.tailnet_endpoint,
		tailnetIpEndpoint: response.claim.tailnet_ip_endpoint,
		relayEndpoint: response.claim.relay_endpoint
	};
}

async function rememberDirectoryMachine(machine: FleetDirectoryMachine): Promise<void> {
	const candidates = directoryCandidates(machine);
	const direct = candidates.find((candidate) => candidate.transport !== 'relay');
	if (!direct || direct.transport === 'relay') return;
	await rememberServerEndpoint(direct.endpoint, {
		systemName: machine.name,
		machineId: machine.id,
		select: false,
		allowPrivateHTTP: direct.transport === 'lan' || direct.transport === 'tailnet-ip',
		lanEndpoint: machine.endpoints_json.lan,
		tailnetEndpoint: machine.endpoints_json.tailnet,
		tailnetIpEndpoint: machine.endpoints_json.tailnet_ip,
		relayEndpoint: machine.endpoints_json.relay,
		transport: direct.transport,
		transportCandidates: candidates,
		sources: ['account'], directoryOnly: true
	});
}

export async function refreshDaemonAccountFleet(): Promise<string[]> {
	const directory = await fetchFleetDirectory();
	if (!directory.signed_in) return [];
	const errors: string[] = [];
	for (const machine of directory.machines) {
		if (!machine.id || machine.id === directory.machine_id) continue;
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
				const response = await claimFleetDirectoryMachine(machine.id);
				const server = await rememberNativeMachineClaim(nativeClaim(response), { select: false });
				await useServers.getState().updateServer(server.id, {
					transportCandidates: directoryCandidates(machine), sources: ['account', 'saved'], directoryOnly: false
				});
				continue;
			} catch (reason) {
				errors.push(`${machine.name}: ${reason instanceof Error ? reason.message : String(reason)}`);
			}
		}
		try {
			await rememberDirectoryMachine(machine);
		} catch (reason) {
			errors.push(`${machine.name}: ${reason instanceof Error ? reason.message : String(reason)}`);
		}
	}
	return errors;
}
