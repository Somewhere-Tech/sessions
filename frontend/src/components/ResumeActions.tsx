import { useEffect, useRef, useState } from 'react';
import {
  cancelContinuationJob,
  fetchContinuationJob,
  previewContinuation,
  startContinuation,
  type ContinuationJob,
  type ContinuationPreview,
  type ResumableSession
} from '../api/sessionsd';
import { adoptConversationWithRepair, adoptionWarning, runAdoptionRepair, type AdoptOutcome } from '../lib/adoptConversation';
import { providerConversationId } from '../lib/sessionStatus';
import { preferNextSessionView } from '../lib/sessionViewPreference';
import { useSessions } from '../store/sessions';
import { formatTokenEstimate, usePaidStartPlan } from '../hooks/usePaidStartPlan';
import { PaidStartPlan, paidStartProviderName } from './PaidStartPlan';
import { ProviderFaultCard } from './ProviderFaultCard';

interface Props {
  selected: ResumableSession | null;
  preferredSourceSessionId?: string;
  preferredDestinationProvider?: 'claude' | 'codex';
  preferredRuntimeMode?: 'rich' | 'terminal';
  onBusyChange: (busy: boolean) => void;
  onResumed: (laneId: string) => void;
  onClose: () => void;
  onStartNew: () => void;
}

type HistoryChoice = 'all' | 'last';

const JOB_STAGES = [
  ['exporting-history', 'Exporting conversation history'],
  ['creating-session', 'Creating the new session'],
  ['provider-starting', 'Starting the agent'],
  ['first-reply', 'Waiting for the first reply']
] as const;
function providerName(provider: 'claude' | 'codex'): string {
  return paidStartProviderName(provider);
}

function sourceSessionID(selected: ResumableSession, preferred: string | undefined): string | undefined {
  if (preferred) return preferred;
  const matches = useSessions.getState().sessions.filter((session) => (
    session.exited
    && providerConversationId(session) === selected.sessionId
    && (selected.tool === 'claude' ? session.tool === 'claude-code' : session.tool === 'codex')
  ));
  return matches.length === 1 ? matches[0]?.id : undefined;
}

function useContinuationPreview(
  selected: ResumableSession | null,
  destination: 'claude' | 'codex',
  historyChoice: HistoryChoice,
  lastMessages: number,
  preferredSourceSessionId?: string
): { preview: ContinuationPreview | null; loading: boolean; error: string | null } {
  const [preview, setPreview] = useState<ContinuationPreview | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (!selected || selected.tool === destination) {
      setPreview(null);
      setLoading(false);
      setError(null);
      return;
    }
    const controller = new AbortController();
    const timer = window.setTimeout(() => {
      setLoading(true);
      setError(null);
      void previewContinuation({
        target: selected.sessionId,
        historyId: selected.historyId,
        sourceSessionId: sourceSessionID(selected, preferredSourceSessionId),
        destinationProvider: destination,
        messageLimit: historyChoice === 'last' ? lastMessages : undefined
      }, controller.signal).then(setPreview).catch((reason: unknown) => {
        if (!controller.signal.aborted) setError(reason instanceof Error ? reason.message : 'Could not measure this conversation.');
      }).finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    }, 150);
    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [destination, historyChoice, lastMessages, preferredSourceSessionId, selected]);
  return { preview, loading, error };
}

function ContinuationProgress({
  job,
  delayed,
  canceling,
  onKeepWaiting,
  onCancel,
  onOpenSession
}: {
  job: ContinuationJob;
  delayed: boolean;
  canceling: boolean;
  onKeepWaiting: () => void;
  onCancel: () => void;
  onOpenSession: () => void;
}): JSX.Element {
  const seen = new Set(job.events.map((event) => event.stage));
  if (job.status === 'canceled') {
    return <div className="continuation-result" role="status"><strong>Continuation canceled</strong><span>{job.stageText}</span></div>;
  }
  if (job.status === 'failed') {
    return <div className="dialog-error" role="alert"><strong>{job.stageText}</strong><span>{job.error}</span></div>;
  }
  return (
    <section className="continuation-progress" aria-live="polite">
      <strong>{delayed ? `${providerName(job.provider)} has not answered yet` : job.stageText}</strong>
      <ol>
        {JOB_STAGES.map(([stage, label]) => (
          <li className={job.stage === stage ? 'is-current' : seen.has(stage) ? 'is-done' : ''} key={stage}>
            <span aria-hidden>{seen.has(stage) && job.stage !== stage ? '✓' : '•'}</span>{label}
          </li>
        ))}
      </ol>
      {job.failureKind && job.laneId ? (
        <ProviderFaultCard
          sessionId={job.laneId}
          failureKind={job.failureKind}
          detail={job.failureDetail}
          retry={job.retry}
          rich
          onOpenTerminal={onOpenSession}
        />
      ) : null}
      <div className="continuation-progress-actions">
        {delayed ? <button type="button" className="btn btn-secondary" onClick={onKeepWaiting}>Keep waiting</button> : null}
        <button type="button" className="btn btn-ghost" disabled={canceling} onClick={onCancel}>
          {canceling ? 'Ending new session…' : 'Cancel'}
        </button>
      </div>
    </section>
  );
}

function HistoryTransferOptions({
  historyChoice,
  lastMessages,
  confirmed,
  preview,
  previewError,
  disabled,
  onHistoryChoice,
  onLastMessages,
  onConfirmed
}: {
  historyChoice: HistoryChoice;
  lastMessages: number;
  confirmed: boolean;
  preview: ContinuationPreview | null;
  previewError: string | null;
  disabled: boolean;
  onHistoryChoice: (value: HistoryChoice) => void;
  onLastMessages: (value: number) => void;
  onConfirmed: (value: boolean) => void;
}): JSX.Element {
  const overThreshold = Boolean(preview && !preview.limited && preview.estimatedTokens > preview.thresholdTokens);
  return (
    <>
      <fieldset className="continuation-history-choice" disabled={disabled}>
        <legend>History to send</legend>
        <label><input type="radio" checked={historyChoice === 'all'} onChange={() => onHistoryChoice('all')} /> <span>The whole conversation</span></label>
        <label><input type="radio" checked={historyChoice === 'last'} onChange={() => onHistoryChoice('last')} /> <span>Only the last <input type="number" min="1" max={preview?.totalMessageCount || undefined} value={lastMessages} disabled={disabled || historyChoice !== 'last'} onChange={(event) => onLastMessages(Math.max(1, Number(event.currentTarget.value) || 1))} /> messages</span></label>
      </fieldset>
      {overThreshold ? (
        <label className="continuation-threshold"><input type="checkbox" checked={confirmed} disabled={disabled} onChange={(event) => onConfirmed(event.currentTarget.checked)} /><span><strong>Send the whole history anyway</strong><small>This is above the {formatTokenEstimate(preview?.thresholdTokens ?? 0)} token warning level.</small></span></label>
      ) : null}
      {previewError ? <div className="dialog-error" role="alert">{previewError}</div> : null}
    </>
  );
}

export function ResumeActions({
  selected,
  preferredSourceSessionId,
  preferredDestinationProvider,
  preferredRuntimeMode,
  onBusyChange,
  onResumed,
  onClose,
  onStartNew
}: Props): JSX.Element {
  const refresh = useSessions((state) => state.refresh);
  const [historyChoice, setHistoryChoice] = useState<HistoryChoice>('all');
  const [lastMessages, setLastMessages] = useState(40);
  const [confirmed, setConfirmed] = useState(false);
  const sourceProvider = selected?.tool ?? 'claude';
  const plan = usePaidStartPlan({
    sourceProvider,
    preferredDestination: preferredDestinationProvider,
    preferredRuntime: preferredRuntimeMode ?? (selected?.transcriptRecovery ? 'rich' : sourceProvider === 'claude' ? 'terminal' : 'rich'),
    terminalAvailable: !selected?.transcriptRecovery
  });
  const { preview, loading: previewLoading, error: previewError } = useContinuationPreview(selected, plan.destination, historyChoice, lastMessages, preferredSourceSessionId);
  const crossProvider = Boolean(selected && selected.tool !== plan.destination);
  const same = useSameProviderResume(selected, preferredSourceSessionId, plan.destination, plan.runtime, plan.model, plan.effort, refresh, onResumed, onClose);
  const continuation = useContinuationRun(refresh, onResumed, onClose);
  const working = crossProvider ? continuation.busy || continuation.job?.status === 'running' : same.busy;
  const error = crossProvider ? continuation.error : same.error;
  useEffect(() => onBusyChange(Boolean(working)), [onBusyChange, working]);
  const overThreshold = Boolean(preview && !preview.limited && preview.estimatedTokens > preview.thresholdTokens);
  const canStartCross = Boolean(preview && !previewError && (!overThreshold || confirmed));
  const primaryText = !selected
    ? 'Choose a conversation'
    : working ? 'Starting…'
    : plan.ready
      ? `Start ${providerName(plan.destination)} (${plan.modelName})`
      : 'Preparing details…';
  const title = selected?.title?.trim() || selected?.firstUserMessage?.trim() || 'Saved conversation';
  const sizeLine = crossProvider
    ? previewLoading
      ? 'Measuring the conversation…'
      : preview
        ? `${preview.messageCount} messages, about ${formatTokenEstimate(preview.estimatedTokens)} tokens`
        : 'Conversation size unavailable'
    : undefined;

  return (
    <>
      {continuation.job ? (
        <ContinuationProgress
          job={continuation.job}
          delayed={continuation.delayed}
          canceling={continuation.busy}
          onKeepWaiting={continuation.keepWaiting}
          onCancel={() => void continuation.cancel()}
          onOpenSession={() => { if (continuation.job?.laneId) onResumed(continuation.job.laneId); }}
        />
      ) : selected ? (
        <PaidStartPlan
          plan={plan}
          title={title}
          sizeLine={sizeLine}
          intro={crossProvider
            ? `${providerName(plan.destination)} starts a new conversation with the history you choose.`
            : `Continues the original ${providerName(selected.tool)} conversation.`}
          sourceNote={crossProvider
            ? `Your ${providerName(selected.tool)} conversation stays unchanged.`
            : `The original ${providerName(selected.tool)} conversation opens again. Nothing is copied.`}
          copyNote={crossProvider
            ? 'Only your messages and the agent’s replies are carried over. Tool output, file changes, attachments, sign-in details, and usage totals stay behind.'
            : undefined}
          disabled={working}
        >
          {crossProvider ? (
            <HistoryTransferOptions
              historyChoice={historyChoice}
              lastMessages={lastMessages}
              confirmed={confirmed}
              preview={preview}
              previewError={previewError}
              disabled={working}
              onHistoryChoice={(choice) => { setHistoryChoice(choice); setConfirmed(false); }}
              onLastMessages={setLastMessages}
              onConfirmed={setConfirmed}
            />
          ) : null}
        </PaidStartPlan>
      ) : null}
      {error ? <div className="dialog-error" role="alert">{error}</div> : null}
      {same.partialResult ? (
        <div className="dialog-warning" role="status" aria-live="assertive">
          <div><strong>The conversation is open, but Sessions could not finish its record.</strong><span>{adoptionWarning(same.partialResult)}</span></div>
          {same.partialResult.repair ? <button type="button" className="btn btn-primary" onClick={() => void same.repair()} disabled={same.busy}>{same.busy ? 'Finishing record…' : 'Finish record'}</button> : null}
        </div>
      ) : null}
      <footer className="resume-dialog-foot">
        <button type="button" className="btn btn-ghost" onClick={onStartNew} disabled={working}>+ New session instead</button>
        <button type="button" className="btn btn-ghost" onClick={onClose} disabled={working}>Close</button>
        {!continuation.job ? <button type="button" className="btn btn-primary continuation-start" onClick={() => {
          if (!selected) return;
          if (crossProvider) void continuation.start({
            target: selected.sessionId, historyId: selected.historyId,
            sourceSessionId: sourceSessionID(selected, preferredSourceSessionId), destinationProvider: plan.destination,
            model: plan.model, effort: plan.effort, messageLimit: historyChoice === 'last' ? lastMessages : undefined,
            confirmWholeHistory: confirmed
          });
          else void same.resume();
        }} disabled={working || !selected || !plan.ready || Boolean(same.partialResult) || (crossProvider && !canStartCross)}>{primaryText}</button> : null}
      </footer>
    </>
  );
}

function useSameProviderResume(
  selected: ResumableSession | null,
  sourceSessionId: string | undefined,
  destination: 'claude' | 'codex',
  runtimeMode: 'rich' | 'terminal',
  model: string,
  effort: string,
  refresh: () => Promise<void>,
  onResumed: (laneId: string) => void,
  onClose: () => void
): { busy: boolean; error: string | null; partialResult: AdoptOutcome | null; resume: () => Promise<void>; repair: () => Promise<void> } {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [partialResult, setPartialResult] = useState<AdoptOutcome | null>(null);
  const resume = async (): Promise<void> => {
    if (!selected || busy) return;
    setBusy(true);
    setError(null);
    try {
      const outcome = await adoptConversationWithRepair(
        selected.sessionId,
        sourceSessionID(selected, sourceSessionId),
        selected.historyId,
        destination,
        runtimeMode,
        undefined,
        model,
        effort,
        'constrained'
      );
      await refresh();
      preferNextSessionView(outcome.result.laneId, runtimeMode === 'terminal' && !outcome.result.transcriptRecovery ? 'terminal' : 'remote');
      onResumed(outcome.result.laneId);
      if (outcome.unresolved || outcome.repairError) setPartialResult(outcome);
      else onClose();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not start this conversation.');
    } finally {
      setBusy(false);
    }
  };
  const repair = async (): Promise<void> => {
    if (!partialResult?.repair || busy) return;
    setBusy(true);
    setError(null);
    try {
      const outcome = await runAdoptionRepair(partialResult.repair);
      await refresh();
      setPartialResult(outcome.unresolved ? outcome : null);
      if (!outcome.unresolved) onClose();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : 'Could not finish the conversation record.');
    } finally {
      setBusy(false);
    }
  };
  return { busy, error, partialResult, resume, repair };
}

function useContinuationRun(
  refresh: () => Promise<void>,
  onResumed: (laneId: string) => void,
  onClose: () => void
): {
  job: ContinuationJob | null; busy: boolean; delayed: boolean; error: string | null;
  start: (request: Parameters<typeof startContinuation>[0]) => Promise<void>;
  cancel: () => Promise<void>; keepWaiting: () => void;
} {
  const [job, setJob] = useState<ContinuationJob | null>(null);
  const [busy, setBusy] = useState(false);
  const [delayed, setDelayed] = useState(false);
  const [waitRound, setWaitRound] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const handledJob = useRef('');
  const jobID = job?.id;
  const jobStatus = job?.status;
  const jobStage = job?.stage;
  useEffect(() => {
    if (!jobID || jobStatus !== 'running') return;
    const poll = window.setInterval(() => void fetchContinuationJob(jobID).then(setJob).catch((reason: unknown) => {
      setError(reason instanceof Error ? reason.message : 'Could not check continuation progress.');
    }), 500);
    return () => window.clearInterval(poll);
  }, [jobID, jobStatus]);
  useEffect(() => {
    if (!job || job.status !== 'succeeded' || !job.laneId || handledJob.current === job.id) return;
    handledJob.current = job.id;
    void refresh().then(() => {
      preferNextSessionView(job.laneId as string, 'remote');
      onResumed(job.laneId as string);
      onClose();
    }).catch((reason: unknown) => {
      setError(reason instanceof Error ? reason.message : 'The new conversation opened, but the list did not refresh.');
    });
  }, [job, onClose, onResumed, refresh]);
  useEffect(() => {
    setDelayed(false);
    if (!jobID || jobStatus !== 'running') return;
    const timer = window.setTimeout(() => setDelayed(true), 60_000);
    return () => window.clearTimeout(timer);
  }, [jobID, jobStage, jobStatus, waitRound]);
  const start = async (request: Parameters<typeof startContinuation>[0]): Promise<void> => {
    if (busy || job?.status === 'running') return;
    setBusy(true);
    setError(null);
    try { setJob(await startContinuation(request)); }
    catch (reason) { setError(reason instanceof Error ? reason.message : 'Could not start this conversation.'); }
    finally { setBusy(false); }
  };
  const cancel = async (): Promise<void> => {
    if (!job || job.status !== 'running' || busy) return;
    setBusy(true);
    try { setJob(await cancelContinuationJob(job.id)); }
    catch (reason) { setError(reason instanceof Error ? reason.message : 'Could not end the new session.'); }
    finally { setBusy(false); }
  };
  return { job, busy, delayed, error, start, cancel, keepWaiting: () => setWaitRound((value) => value + 1) };
}
