import { useState, type JSX } from 'react';
import type { SessionInfo } from '../types';
import { type InboxLayout, type InboxSection, notConnectedReason } from '../lib/inboxSections';
import { resolvedSessionLabel } from '../lib/tabLabels';
import { ProviderMark, normalizeProvider } from './ProviderBadge';

interface Props {
  layout: InboxLayout;
  renderNode: (session: SessionInfo, endedFlat?: boolean) => JSX.Element | null;
  onOpen: (id: string) => void;
  onShowAllNeedsYou: () => void;
  folderOf: (session: SessionInfo) => string;
  relativeTime: (at: number) => string;
  lastActivity: (session: SessionInfo) => number;
}

// The inbox body: a needs-you strip, then one section per named project,
// then everything unnamed under Other projects. Rows come from the
// navigator's own renderer so pins, drag, menus, and child folding behave the
// same everywhere; this component only decides where each row sits.
export function InboxSections({ layout, renderNode, onOpen, onShowAllNeedsYou, folderOf, relativeTime, lastActivity }: Props) {
  const sections = layout.other ? [...layout.sections, layout.other] : layout.sections;
  return (
    <>
      {layout.needsYou.length > 0 ? (
        <div className="session-tree-group inbox-needs" role="group" aria-label="Sessions waiting on you">
          <div className="session-tree-group-head is-attention"><span>Needs you</span><strong>{layout.needsYou.length + layout.moreNeedsYou}</strong></div>
          {layout.needsYou.map((session) => {
            const provider = normalizeProvider(session.tool);
            return (
              <button type="button" className="inbox-needs-row" key={session.id} onClick={() => onOpen(session.id)} title="Open this session">
                <span className="inbox-needs-mark">{provider ? <ProviderMark provider={provider} size={16} /> : <span aria-hidden>⌘</span>}</span>
                <span className="inbox-needs-copy">
                  <strong>{resolvedSessionLabel(session)}</strong>
                  <small>{folderOf(session)}</small>
                </span>
                <span className="inbox-needs-why">{session.idleDetail || 'Waiting for you'}</span>
                <time>{relativeTime(lastActivity(session))}</time>
              </button>
            );
          })}
          {layout.moreNeedsYou > 0 ? (
            <button type="button" className="inbox-fold" onClick={onShowAllNeedsYou}>
              <span>{layout.moreNeedsYou} more waiting</span><small>show all</small>
            </button>
          ) : null}
        </div>
      ) : null}
      {sections.map((section) => (
        <ProjectSection key={section.id} section={section} renderNode={renderNode} />
      ))}
    </>
  );
}

function ProjectSection({ section, renderNode }: { section: InboxSection; renderNode: Props['renderNode'] }) {
  const [open, setOpen] = useState(true);
  const [finishedOpen, setFinishedOpen] = useState(false);
  const [notConnectedOpen, setNotConnectedOpen] = useState(false);
  const count = section.live.length + section.notConnected.length;
  return (
    <div className={`session-tree-group inbox-project${section.implicit ? ' is-implicit' : ''}`}>
      <button type="button" className="session-tree-group-head inbox-project-head" onClick={() => setOpen((current) => !current)} aria-expanded={open}>
        <span className="session-group-disclosure">
          <span className={`inbox-chevron${open ? ' is-open' : ''}`} aria-hidden>▸</span>
          {section.name}
        </span>
        <strong>
          {section.needsYou > 0 ? <em className="inbox-project-needs">{section.needsYou} needs you · </em> : null}
          {count}
        </strong>
      </button>
      {open ? (
        <>
          {section.live.map((session) => renderNode(session))}
          {section.live.length === 0 && section.notConnected.length === 0 ? <div className="session-tree-empty is-compact">Nothing live here.</div> : null}
          {section.notConnected.length > 0 ? (
            <>
              <button type="button" className="inbox-fold" onClick={() => setNotConnectedOpen((current) => !current)} aria-expanded={notConnectedOpen}>
                <span>{notConnectedOpen ? '▾' : '▸'} Not connected · {section.notConnected.length}</span>
                <small>{notConnectedReason(section.notConnected[0]!)}</small>
              </button>
              {notConnectedOpen ? section.notConnected.map((session) => renderNode(session)) : null}
            </>
          ) : null}
          {section.finished.length > 0 ? (
            <>
              <button type="button" className="inbox-fold" onClick={() => setFinishedOpen((current) => !current)} aria-expanded={finishedOpen}>
                <span>{finishedOpen ? '▾' : '▸'} Finished · {section.finished.length}</span>
                <small>{resolvedSessionLabel(section.finished[0]!)}{section.finished.length > 1 ? ` · +${section.finished.length - 1}` : ''}</small>
              </button>
              {finishedOpen ? section.finished.map((session) => renderNode(session, true)) : null}
            </>
          ) : null}
        </>
      ) : null}
    </div>
  );
}
