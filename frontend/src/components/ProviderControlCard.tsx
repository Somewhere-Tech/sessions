import type { SnapshotComposerState, TrustChoice } from '../lib/detectMultiChoice';

interface Props {
  // The daemon's durable needs-input line, when it has one.
  detail: string | null;
  // What the terminal snapshot classifier saw, when it saw a control.
  blockingState: SnapshotComposerState | null;
  trustChoice: TrustChoice | null;
  // Sends raw keystrokes in order with a short gap so the TUI can redraw.
  answer?: (keys: string[]) => void;
  onOpenTerminal: () => void;
}

// A provider control (Claude's folder-trust dialog, a picker, a confirmation)
// is answered here, in the conversation, with the choices the terminal is
// showing. Typing a message into a control activates whichever option is
// highlighted, so the daemon refuses sends while one is open and this card is
// the way through.
export function ProviderControlCard({ detail, blockingState, trustChoice, answer, onOpenTerminal }: Props) {
  if (!detail && !blockingState) return null;
  const text = detail ?? blockingState?.description ?? 'The provider is waiting for a choice.';
  return (
    <div className="provider-control-card" role="group" aria-label="Provider control">
      <span className="provider-control-card-title">Needs you</span>
      <p className="provider-control-card-text">{text}</p>
      {answer && blockingState?.kind === 'trust-prompt' && trustChoice ? (
        <div className="provider-control-card-choices">
          <button
            type="button"
            className="provider-control-card-action is-primary"
            onClick={() => answer(trustChoice.selected === 'yes' ? ['\r'] : ['\x1b[B', '\r'])}
          >
            Yes, I trust this folder
          </button>
          <button
            type="button"
            className="provider-control-card-action"
            onClick={() => answer(trustChoice.selected === 'no' ? ['\r'] : ['\x1b[A', '\r'])}
          >
            No, exit
          </button>
        </div>
      ) : answer ? (
        <div className="provider-control-card-choices" role="toolbar" aria-label="Answer the provider control">
          <button type="button" className="provider-control-card-action" onClick={() => answer(['\x1b[A'])} aria-label="Move up">↑</button>
          <button type="button" className="provider-control-card-action" onClick={() => answer(['\x1b[B'])} aria-label="Move down">↓</button>
          <button type="button" className="provider-control-card-action is-primary" onClick={() => answer(['\r'])}>Enter</button>
          <button type="button" className="provider-control-card-action" onClick={() => answer(['\x1b'])}>Esc</button>
        </div>
      ) : null}
      <button type="button" className="provider-control-card-terminal" onClick={onOpenTerminal}>
        {answer ? 'Show the exact terminal' : 'Open Terminal view to respond'}
      </button>
    </div>
  );
}
