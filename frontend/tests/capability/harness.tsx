// The shared mount for capability tests that need more than one surface.
//
// This mirrors the wiring App.tsx does — the navigator's `sessions` prop comes
// from the sessions store, the dialogs get the store's create/refresh — so a
// test that "creates a session and sees it in the list" is exercising the same
// path the product uses. It is not a stand-in for a component and it stubs
// nothing: every child here is the real component.
import { useEffect, type ReactNode } from 'react';
import { SessionNavigator } from '../../src/components/SessionNavigator';
import { useSessions } from '../../src/store/sessions';

export interface WorkbenchProps {
  machine?: string;
  children?: ReactNode;
  onOpen?: (id: string) => void;
  onNew?: () => void;
}

/**
 * The operations navigator, fed from the sessions store exactly as App.tsx
 * feeds it. Anything a test renders in `children` sits alongside it, so an
 * action taken in a dialog is observable in the list.
 */
export function Workbench({
  machine = 'Fixture Mac',
  children,
  onOpen = () => {},
  onNew = () => {}
}: WorkbenchProps): JSX.Element {
  const sessions = useSessions((state) => state.sessions);
  const activeId = useSessions((state) => state.activeId);
  const refresh = useSessions((state) => state.refresh);

  // App.tsx refreshes on mount; without it the list would only ever show what
  // a test seeded, and "the daemon now has one more session" would be
  // untestable.
  useEffect(() => { void refresh(); }, [refresh]);

  return (
    <div className="app-shell operations-shell" data-theme="dark">
      <SessionNavigator
        sessions={sessions}
        activeId={activeId}
        machine={machine}
        onOpen={onOpen}
        onOpenMachineSession={() => {}}
        onNew={onNew}
        onContinue={() => {}}
        onResumeSession={() => {}}
        onForkSession={async () => {}}
        onStartLinked={() => {}}
        openSessionIds={[]}
        onCloseView={() => {}}
        onReparent={async () => {}}
      />
      {children}
    </div>
  );
}

/**
 * The Ended group's disclosure. The navigator also has a filter chip whose
 * accessible name is exactly "Ended"; the group head carries its count, so this
 * distinguishes the two without reaching for a class name.
 */
export const ENDED_GROUP = /^Ended \d+$/;
