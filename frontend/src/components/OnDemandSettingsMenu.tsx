import { lazy, Suspense, useState } from 'react';
import type { SettingsMenuProps } from './SettingsMenu';

const SettingsMenu = lazy(() => import('./SettingsMenu').then((module) => ({ default: module.SettingsMenu })));

function Trigger({ onClick }: { onClick: () => void }): JSX.Element {
  return (
    <div className="settings-menu">
      <button
        type="button"
        className="settings-menu-trigger"
        onClick={onClick}
        aria-haspopup="menu"
        aria-expanded={false}
        title="Settings"
      >
        ⚙
      </button>
    </div>
  );
}

export function OnDemandSettingsMenu(props: SettingsMenuProps): JSX.Element {
  const [requested, setRequested] = useState(false);
  if (!requested) return <Trigger onClick={() => setRequested(true)} />;
  return (
    <Suspense fallback={<Trigger onClick={() => undefined} />}>
      <SettingsMenu {...props} initiallyOpen />
    </Suspense>
  );
}
