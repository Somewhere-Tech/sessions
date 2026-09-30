import type { TeamMember } from '../api/sessionsd';
import type { SessionInfo } from '../types';
import { StartReceiptNote } from './StartReceiptNote';
import '../styles/team-handoff.css';

export function TeamEvidence({ member, session }: { member?: TeamMember; session: SessionInfo }): JSX.Element {
  const receipt = member?.handoff;
  return <>
    <StartReceiptNote session={{ ...session, start: member?.start ?? session.start }} />
    {session.branch ? <p className="subagent-branch" title={session.worktreePath}>
      {session.exited ? 'Kept branch ' : 'On branch '}<code>{session.branch}</code>
    </p> : null}
    {member?.checkout_warning ? <p className="team-checkout-warning" role="status">
      {member.checkout_warning.detail}
    </p> : null}
    {receipt ? <details className="team-handoff">
      <summary>Handoff{receipt.source === 'not-reported' ? ' · not reported' : ` · ${receipt.outcome ?? 'reported'}`}</summary>
      <p>{receipt.source === 'agent-reported'
        ? 'Reported by the agent; not independently verified by Sessions.'
        : 'No handoff reported yet. A finished turn does not prove the task is complete.'}</p>
      {receipt.at && Number.isFinite(Date.parse(receipt.at)) ? <p>Reported <time dateTime={receipt.at}>
        {new Date(receipt.at).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })}
      </time></p> : null}
      {receipt.summary ? <p>{receipt.summary}</p> : null}
      <dl>
        <dt>Work lives in</dt><dd>{receipt.workspace || 'Not recorded'}</dd>
        <dt>Branch</dt><dd>{receipt.branch || 'Not recorded'}</dd>
        <dt>Push</dt><dd>{receipt.push === 'pushed' ? 'Reported pushed' : receipt.push === 'not-pushed' ? 'Not pushed' : 'Not reported'}</dd>
      </dl>
      <ReceiptList title="Commits" items={receipt.commits} />
      <ReceiptList title="Tests" items={receipt.tests} />
      <ReceiptList title="Artifacts" items={receipt.artifacts} />
      <ReceiptList title="Remaining" items={receipt.remaining} />
      {receipt.detail ? <p role="status">{receipt.detail}</p> : null}
    </details> : null}
  </>;
}

function ReceiptList({ title, items }: { title: string; items?: string[] }): JSX.Element {
  return <div className="team-handoff-section"><strong>{title}</strong>
    {items?.length ? <ul>{items.map((item, index) => <li key={index}>{item}</li>)}</ul> : <p>Not reported</p>}
  </div>;
}
