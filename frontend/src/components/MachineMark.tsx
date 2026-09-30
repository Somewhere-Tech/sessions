interface Props {
  machine: string;
  size?: number;
  platform?: string;
}

export type MachinePlatform = 'darwin' | 'macos' | 'windows' | 'linux' | 'server';

export function MachinePlatformIcon({ platform }: { platform: MachinePlatform }): JSX.Element {
  if (platform === 'darwin' || platform === 'macos') {
    return <svg viewBox="0 0 20 20" aria-hidden><path d="M3.25 4.25h13.5v9H3.25v-9Zm-1.5 11.5h16.5M7.25 15.75h5.5" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" /></svg>;
  }
  if (platform === 'windows') {
    return <svg viewBox="0 0 20 20" aria-hidden><path d="M2.25 3.7 9 2.8v6.45H2.25V3.7Zm7.75-1.02 7.75-1.05v7.62H10V2.68ZM2.25 10.2H9v6.55l-6.75-.93V10.2Zm7.75 0h7.75v7.62L10 16.76V10.2Z" /></svg>;
  }
  if (platform === 'linux') {
    return <svg viewBox="0 0 20 20" aria-hidden><path d="M10 2.2c-2.1 0-3.55 1.9-3.55 4.55 0 .8.14 1.48.35 2.04-.55.7-1.27 1.76-1.72 2.95-.7 1.84-.17 3.14.79 3.45.5.16 1.08-.02 1.59-.43.73.7 1.57 1.04 2.54 1.04.98 0 1.83-.35 2.56-1.05.5.42 1.08.6 1.58.44.96-.31 1.49-1.61.79-3.45-.45-1.19-1.17-2.25-1.72-2.95.2-.56.34-1.24.34-2.04C13.55 4.1 12.1 2.2 10 2.2Z" /><circle cx="8.7" cy="6.35" r=".65" fill="var(--bg)" /><circle cx="11.3" cy="6.35" r=".65" fill="var(--bg)" /></svg>;
  }
  return <svg viewBox="0 0 20 20" aria-hidden><path d="M3 3.5h14v5H3v-5Zm0 8h14v5H3v-5Z" fill="none" stroke="currentColor" strokeWidth="1.4" /><path d="M6 6h.01M6 14h.01" stroke="currentColor" strokeWidth="2" strokeLinecap="round" /></svg>;
}

function inferredPlatform(machine: string, explicit?: string): 'darwin' | 'windows' | 'linux' {
  const value = `${explicit ?? ''} ${machine}`.toLowerCase();
  if (value.includes('win')) return 'windows';
  if (value.includes('linux') || value.includes('ubuntu') || value.includes('debian')) return 'linux';
  if ((machine === 'This machine' || machine.includes('(this machine)')) && typeof navigator !== 'undefined') {
    const agent = navigator.userAgent.toLowerCase();
    if (agent.includes('windows')) return 'windows';
    if (agent.includes('linux') && !agent.includes('android')) return 'linux';
  }
  return 'darwin';
}

export function MachineMark({ machine, size = 17, platform }: Props): JSX.Element {
  const kind = inferredPlatform(machine, platform);
  return (
    <span
      className={`machine-mark is-${kind}`}
      style={{ width: size, height: size }}
      role="img"
      aria-label={machine}
      title={machine}
    >
      <MachinePlatformIcon platform={kind} />
    </span>
  );
}
