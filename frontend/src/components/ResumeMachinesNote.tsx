import type { OtherMachine } from '../hooks/useOtherMachines';

function names(machines: OtherMachine[]): string {
  const list = machines.map((machine) => machine.name);
  if (list.length <= 2) return list.join(' and ');
  return `${list.slice(0, -1).join(', ')}, and ${list[list.length - 1]}`;
}

// Says, in one line, why a conversation from another machine is not in this
// list: the picker reads this machine's history, and a machine that is offline
// cannot be read at all. Nothing to say when Fleet has only this machine.
export function resumeMachinesLine(machines: OtherMachine[]): string | null {
  if (machines.length === 0) return null;
  const offline = machines.filter((machine) => machine.reachability === 'unreachable');
  const online = machines.filter((machine) => machine.reachability !== 'unreachable');
  const parts: string[] = [];
  if (online.length > 0) {
    parts.push(`Conversations on ${names(online)} are listed in Fleet.`);
  }
  if (offline.length > 0) {
    parts.push(`${names(offline)} ${offline.length === 1 ? 'is' : 'are'} offline right now, so ${offline.length === 1 ? 'its' : 'their'} conversations cannot be picked up until ${offline.length === 1 ? 'it is' : 'they are'} back.`);
  }
  return parts.join(' ');
}

export function ResumeMachinesNote({ machines }: { machines: OtherMachine[] }) {
  const line = resumeMachinesLine(machines);
  if (!line) return null;
  const offline = machines.some((machine) => machine.reachability === 'unreachable');
  return <p className={`resume-machines-note${offline ? ' is-offline' : ''}`}>{line}</p>;
}
