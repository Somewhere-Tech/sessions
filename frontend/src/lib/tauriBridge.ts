// Thin wrapper around Tauri APIs that survives running in a regular
// browser too. The desktop app loads the same React bundle as the
// browser; we feature-detect window.__TAURI_INTERNALS__ to decide
// whether to call invoke()/native plugins or fall back to web APIs
// (window.open, web Notification, etc.).
//
// All async — even in the browser path — so call sites don't have to
// branch on the runtime.

export const isTauri = (): boolean =>
  typeof window !== 'undefined' &&
  (window as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__ !== undefined;

// Tauri 2: invoke() comes from @tauri-apps/api/core. We dynamic-import so
// the package is only loaded when actually running inside Tauri (avoids
// shipping the module to the phone PWA where it'd be dead weight).
async function invoke<T>(cmd: string, args?: Record<string, unknown>): Promise<T> {
  const mod = await import('@tauri-apps/api/core');
  return mod.invoke<T>(cmd, args);
}

// Pop a single session into its own native window (Tauri) or a new
// browser tab (web). The Tauri command focuses an existing window for
// the same session id rather than stacking duplicates.
//
// Browser path opens as a TAB (no width/height in features) — Chrome
// and Safari treat any `window.open` with explicit dims as a popup,
// which is blocked by default popup-blocker rules even from a real
// user gesture. As a plain `_blank`, the browser opens it as a tab
// and the user can drag it out into its own window if they want.
export async function openSessionWindow(sessionId: string, title: string): Promise<void> {
  const query = new URLSearchParams({ session: sessionId, mode: 'single' }).toString();
  if (isTauri()) {
    await invoke<void>('open_scoped_window', { query, title });
    return;
  }
  const url = `${window.location.origin}${window.location.pathname}?${query}`;
  const w = window.open(url, '_blank', 'noopener,noreferrer');
  if (!w) {
    // Last-resort fallback — some hardened browsers block window.open
    // entirely from background contexts. Force-navigate the current
    // tab; the user can use the back button to return.
    console.warn('window.open returned null — popup blocker? Falling back to same-tab nav.');
  }
}

// Fire a browser notification for working→idle transitions. The native app
// intentionally leaves notifications to the daemon-hosted web push path.
export async function notify(title: string, body: string): Promise<void> {
  // v1 deliberately does not forward web activity into native
  // notifications. The daemon-hosted web push path remains authoritative,
  // which avoids duplicate alerts when the desktop shell is also open.
  if (isTauri()) return;
  if (typeof Notification === 'undefined') return;
  if (Notification.permission === 'granted') {
    new Notification(title, { body });
  } else if (Notification.permission !== 'denied') {
    const perm = await Notification.requestPermission();
    if (perm === 'granted') new Notification(title, { body });
  }
}

export interface TrayServer {
  id: string;
  name: string;
}

export async function syncTrayServers(servers: TrayServer[]): Promise<void> {
  if (!isTauri()) return;
  await invoke<void>('set_tray_servers', {
    servers: servers.map(({ id, name }) => ({ id, name }))
  });
}

export interface NativeMachineCredential {
  serverId: string;
  token: string;
}

export interface NativeMachineCredentialStore {
  supported: boolean;
  credentials: NativeMachineCredential[];
}

export async function loadNativeMachineCredentials(): Promise<NativeMachineCredentialStore> {
  if (!isTauri()) return { supported: false, credentials: [] };
  return invoke<NativeMachineCredentialStore>('native_machine_credentials_load');
}

export async function saveNativeMachineCredentials(
  credentials: NativeMachineCredential[]
): Promise<NativeMachineCredentialStore> {
  if (!isTauri()) return { supported: false, credentials: [] };
  return invoke<NativeMachineCredentialStore>('native_machine_credentials_save', { credentials });
}

export interface NativeSavedMachine {
  alias: string;
  machine_id: string;
  name: string;
  endpoint: string;
  transport: string;
  device_id: string;
  connected_at: string;
}

export interface NativeMovePlan {
  source_id: string;
  target_id?: string;
  target_endpoint: string;
  tool: string;
  cwd: string;
  resume_recipe: string[];
  runtime_mode: 'rich' | 'terminal';
  conversation_bytes: number;
  dry_run: boolean;
  target_lineage_recorded?: boolean;
  warning?: string;
  workspace: {
    git: boolean;
    root?: string;
    remote_name?: string;
    remote_url?: string;
    branch?: string;
    revision?: string;
    checkpoint_ref?: string;
    dirty?: boolean;
  };
}

export async function listNativeMoveMachines(): Promise<NativeSavedMachine[]> {
  if (!isTauri()) return [];
  const result = await invoke<NativeConnectionCommand<{ machines: NativeSavedMachine[] }>>(
    'native_move_machines'
  );
  return result.data.machines ?? [];
}

export async function moveNativeSession(
  sessionId: string,
  machine: string,
  options: { dryRun: boolean; allowDirty?: boolean; runtimeMode?: 'rich' | 'terminal' }
): Promise<NativeMovePlan> {
  if (!isTauri()) throw new Error('Cross-machine continuation is available in Sessions.app');
  const result = await invoke<NativeConnectionCommand<NativeMovePlan>>('native_move_session', {
    sessionId,
    machine,
    dryRun: options.dryRun,
    allowDirty: options.allowDirty ?? false,
    runtimeMode: options.runtimeMode ?? 'rich'
  });
  return result.data;
}

export interface NativeRuntimeStatus {
  state: 'ready' | 'development' | 'client-only' | 'disabled' | 'error';
  detail: string;
  serviceLabel: string;
  runtimeVersion: string | null;
}

export interface NativeConnectionSettings {
  port: number;
  runtime: NativeRuntimeStatus;
}

export interface NativeConnectionCommand<T = Record<string, unknown>> {
  data: T;
  detail: string;
}

export async function getNativeConnectionSettings(): Promise<NativeConnectionSettings | null> {
  if (!isTauri()) return null;
  return invoke<NativeConnectionSettings>('native_connection_settings');
}

export async function setNativeRuntimePort(port: number): Promise<NativeConnectionSettings> {
  if (!isTauri()) throw new Error('The browser client cannot reconfigure the local background service');
  return invoke<NativeConnectionSettings>('set_runtime_port', { port });
}

export async function runNativeConnectionAction<T = Record<string, unknown>>(
  kind: 'lan' | 'remote' | 'pair',
  action: 'status' | 'enable' | 'disable' | 'create',
  name?: string
): Promise<NativeConnectionCommand<T>> {
  if (!isTauri()) throw new Error('This connection action is available in Sessions.app');
  return invoke<NativeConnectionCommand<T>>('native_connection_action', { kind, action, name });
}

export interface NativePairingClaim {
  endpoint: string;
  machineId: string;
  machineName: string;
  deviceId: string;
  token: string;
  name: string;
}

export async function claimNativePairingLink(pairUrl: string): Promise<NativePairingClaim> {
  if (!isTauri()) throw new Error('Cross-machine pairing is available in Sessions.app');
  return invoke<NativePairingClaim>('native_pairing_claim', { pairUrl });
}

export interface NativeTailnetPeer {
  endpoint: string;
  hostname: string;
  os: string;
}

export interface NativeNearbyPeer {
  endpoint: string;
  hostname: string;
  name: string;
  address: string;
  port: number;
  transport: 'nearby';
  version: string;
  os: string;
  arch: string;
  sessions_loaded: number;
  reachable: boolean;
}

export interface NativeMachinePeer {
  endpoint: string;
  hostname: string;
  name: string;
  os: string;
  transport: 'tailnet' | 'nearby';
}

export interface NativeTailnetRequest {
  endpoint: string;
  requestId: string;
  requestSecret: string;
  expiresAt: string;
  status: 'pending';
}

export interface NativeTailnetClaim {
  status: 'pending' | 'accepted' | 'denied' | 'expired';
  claim: NativePairingClaim | null;
}

export async function discoverNativeTailnetPeers(): Promise<NativeTailnetPeer[]> {
  if (!isTauri()) throw new Error('Tailnet discovery is available in Sessions.app');
  return invoke<NativeTailnetPeer[]>('native_tailnet_discover');
}

export async function discoverNativeNearbyPeers(): Promise<NativeNearbyPeer[]> {
  if (!isTauri()) throw new Error('Nearby discovery is available in Sessions.app');
  return invoke<NativeNearbyPeer[]>('native_nearby_discover');
}

export async function requestNativeTailnetAccess(
  endpoint: string,
  clientId: string,
  name: string
): Promise<NativeTailnetRequest> {
  if (!isTauri()) throw new Error('Tailnet access requests are available in Sessions.app');
  return invoke<NativeTailnetRequest>('native_tailnet_request', { endpoint, clientId, name });
}

export async function claimNativeTailnetAccess(
  request: Pick<NativeTailnetRequest, 'endpoint' | 'requestId' | 'requestSecret'>
): Promise<NativeTailnetClaim> {
  if (!isTauri()) throw new Error('Tailnet access requests are available in Sessions.app');
  return invoke<NativeTailnetClaim>('native_tailnet_claim', request);
}

export async function requestNativeNearbyAccess(
  endpoint: string,
  clientId: string,
  name: string
): Promise<NativeTailnetRequest> {
  if (!isTauri()) throw new Error('Nearby access requests are available in Sessions.app');
  return invoke<NativeTailnetRequest>('native_nearby_request', { endpoint, clientId, name });
}

export async function claimNativeNearbyAccess(
  request: Pick<NativeTailnetRequest, 'endpoint' | 'requestId' | 'requestSecret'>
): Promise<NativeTailnetClaim> {
  if (!isTauri()) throw new Error('Nearby access requests are available in Sessions.app');
  return invoke<NativeTailnetClaim>('native_nearby_claim', request);
}

export async function discoverNativeMachines(): Promise<{
  peers: NativeMachinePeer[];
  errors: string[];
}> {
  const [tailnet, nearby] = await Promise.allSettled([
    discoverNativeTailnetPeers(),
    discoverNativeNearbyPeers()
  ]);
  const candidates: NativeMachinePeer[] = [
    ...(tailnet.status === 'fulfilled'
      ? tailnet.value.map((peer) => ({ ...peer, name: peer.hostname, transport: 'tailnet' as const }))
      : []),
    ...(nearby.status === 'fulfilled'
      ? nearby.value.map((peer) => ({
          endpoint: peer.endpoint,
          hostname: peer.hostname,
          name: peer.name,
          os: peer.os,
          transport: 'nearby' as const
        }))
      : [])
  ];
  const byIdentity = new Map<string, NativeMachinePeer>();
  for (const peer of candidates) {
    const identity = (peer.hostname || peer.name).trim().toLowerCase();
    const existing = byIdentity.get(identity);
    if (!existing || (existing.transport === 'nearby' && peer.transport === 'tailnet')) {
      byIdentity.set(identity, peer);
    }
  }
  const errors = [tailnet, nearby]
    .filter((result): result is PromiseRejectedResult => result.status === 'rejected')
    .map((result) => result.reason instanceof Error ? result.reason.message : String(result.reason));
  return {
    peers: [...byIdentity.values()].sort((left, right) => left.name.localeCompare(right.name)),
    errors
  };
}

export async function requestNativeMachineAccess(
  peer: Pick<NativeMachinePeer, 'endpoint' | 'transport'>,
  clientId: string,
  name: string
): Promise<NativeTailnetRequest> {
  return peer.transport === 'nearby'
    ? requestNativeNearbyAccess(peer.endpoint, clientId, name)
    : requestNativeTailnetAccess(peer.endpoint, clientId, name);
}

export async function claimNativeMachineAccess(
  transport: NativeMachinePeer['transport'],
  request: Pick<NativeTailnetRequest, 'endpoint' | 'requestId' | 'requestSecret'>
): Promise<NativeTailnetClaim> {
  return transport === 'nearby'
    ? claimNativeNearbyAccess(request)
    : claimNativeTailnetAccess(request);
}

export interface BackupStatus {
  enabled: boolean;
  encrypt: boolean;
  key_path?: string;
  project?: string;
  interval?: string;
  last_push_at?: string;
  last_push_count: number;
  last_push_skipped: number;
  last_session_count: number;
}

export interface BackupEnableResult extends BackupStatus {
  recovery_phrase?: string;
  key_reused?: boolean;
}

export interface BackupPushResult {
  pushed_at: string;
  uploaded: number;
  skipped: number;
  session_count: number;
  unresolved: number;
  manifest_path: string;
}

export async function runNativeBackupAction<T = BackupStatus>(
  action: 'status' | 'enable' | 'now',
  project?: string
): Promise<NativeConnectionCommand<T>> {
  if (!isTauri()) throw new Error('Encrypted Somewhere backup is available in Sessions.app');
  return invoke<NativeConnectionCommand<T>>('native_backup_action', { action, project });
}

export async function getNativeRuntimeStatus(): Promise<NativeRuntimeStatus | null> {
  if (!isTauri()) return null;
  return invoke<NativeRuntimeStatus>('runtime_status');
}

export interface SomewhereCLIStatus {
  installed: boolean;
  installedVersion: string | null;
  latestVersion: string | null;
  updateAvailable: boolean;
  installCommand: string;
  updateCommand: string;
  detail: string;
}

export async function getSomewhereCLIStatus(): Promise<SomewhereCLIStatus | null> {
  if (!isTauri()) return null;
  return invoke<SomewhereCLIStatus>('somewhere_cli_status');
}

export interface SupportDiagnostics {
  generated_at: string;
  cli_version: string;
  os: string;
  arch: string;
  daemon: {
    reachable: boolean;
    ok: boolean;
    version?: string;
    discovering?: boolean;
    sessions_loaded?: number;
  };
}

export interface SupportPreview {
  schema_version: number;
  ticket_url: string;
  feedback_url: string;
  bug_url: string;
  security_url: string;
  agent: {
    machine_readable_command: string;
    user_approval_required: true;
    automatic_submission: false;
    capture: string[];
  };
  diagnostics?: SupportDiagnostics;
  excluded: string[];
  uploaded: false;
}

const supportURLs = {
  choose: 'https://github.com/Somewhere-Tech/sessions/issues/new/choose',
  feedback: 'https://github.com/Somewhere-Tech/sessions/issues/new?template=feedback.yml',
  bug: 'https://github.com/Somewhere-Tech/sessions/issues/new?template=bug_report.yml',
  security: 'https://github.com/Somewhere-Tech/sessions/security/advisories/new'
} as const;

export type SupportPage = keyof typeof supportURLs;

export async function getNativeSupportPreview(): Promise<SupportPreview> {
  if (!isTauri()) throw new Error('The diagnostic preview is available in Sessions.app');
  const result = await invoke<NativeConnectionCommand<SupportPreview>>('native_support_preview');
  return result.data;
}

export async function openSupportPage(kind: SupportPage): Promise<void> {
  if (isTauri()) {
    await invoke<void>('open_support_page', { kind });
    return;
  }
  const target = supportURLs[kind];
  const opened = window.open(target, '_blank', 'noopener,noreferrer');
  if (!opened) window.location.assign(target);
}

export interface NativeUpdateInfo {
  currentVersion: string;
  version: string;
  notes: string | null;
  publishedAt: string | null;
}

export interface NativeUpdateProgress {
  downloadedBytes: number;
  totalBytes: number | null;
}

type PendingUpdate = Awaited<ReturnType<typeof import('@tauri-apps/plugin-updater')['check']>>;

let pendingUpdate: PendingUpdate = null;
let pendingUpdateCheck: Promise<NativeUpdateInfo | null> | null = null;

export async function checkForNativeUpdate(): Promise<NativeUpdateInfo | null> {
  if (!isTauri()) return null;
  if (pendingUpdateCheck) return pendingUpdateCheck;
  pendingUpdateCheck = (async () => {
    if (pendingUpdate) {
      await pendingUpdate.close();
      pendingUpdate = null;
    }
    const { check } = await import('@tauri-apps/plugin-updater');
    pendingUpdate = await check({ timeout: 12_000 });
    if (!pendingUpdate) return null;
    return {
      currentVersion: pendingUpdate.currentVersion,
      version: pendingUpdate.version,
      notes: pendingUpdate.body ?? null,
      publishedAt: pendingUpdate.date ?? null
    };
  })();
  try {
    return await pendingUpdateCheck;
  } finally {
    pendingUpdateCheck = null;
  }
}

export async function notifyNativeUpdate(info: NativeUpdateInfo): Promise<void> {
  if (!isTauri()) return;
  const notification = await import('@tauri-apps/plugin-notification');
  let granted = await notification.isPermissionGranted();
  if (!granted) {
    granted = (await notification.requestPermission()) === 'granted';
  }
  if (!granted) return;
  notification.sendNotification({
    title: `Sessions ${info.version} is ready`,
    body: 'Open Sessions Settings to review the notes and update. Running sessions will keep working.'
  });
}

export async function notifyProviderUpdate(name: string, version: string): Promise<void> {
  if (!isTauri()) return;
  const notification = await import('@tauri-apps/plugin-notification');
  let granted = await notification.isPermissionGranted();
  if (!granted) {
    granted = (await notification.requestPermission()) === 'granted';
  }
  if (!granted) return;
  notification.sendNotification({
    title: `${name} ${version} is available`,
    body: `Open Sessions Settings to update ${name}. Running agents will not be restarted.`
  });
}

export async function installNativeUpdate(
  onProgress?: (progress: NativeUpdateProgress) => void
): Promise<void> {
  if (!isTauri()) throw new Error('Updates are available only in Sessions.app');
  if (!pendingUpdate) throw new Error('Check for an update first');

  let downloadedBytes = 0;
  let totalBytes: number | null = null;
  await pendingUpdate.downloadAndInstall((event) => {
    if (event.event === 'Started') {
      downloadedBytes = 0;
      totalBytes = event.data.contentLength ?? null;
    } else if (event.event === 'Progress') {
      downloadedBytes += event.data.chunkLength;
    } else if (event.event === 'Finished' && totalBytes !== null) {
      downloadedBytes = totalBytes;
    }
    onProgress?.({ downloadedBytes, totalBytes });
  });

  // Relaunch only the UI process. The daemon and every runner are launchd-owned
  // and remain alive across this replacement.
  const { relaunch } = await import('@tauri-apps/plugin-process');
  await relaunch();
}
