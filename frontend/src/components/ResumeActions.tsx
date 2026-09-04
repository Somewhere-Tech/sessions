import { useEffect, useRef, useState } from 'react';
import {
  cancelContinuationJob,
  fetchContinuationJob,
  fetchProviderStatuses,
  previewContinuation,
  startContinuation,
  type ContinuationJob,
  type ContinuationPreview,
  type ResumableSession,
  type SessionModelOption
} from '../api/sessionsd';
import { adoptConversationWithRepair, adoptionWarning, runAdoptionRepair, type AdoptOutcome } from '../lib/adoptConversation';
import { providerConversationId } from '../lib/sessionStatus';
import { useSessions } from '../store/sessions';
import { ModelPicker, type ModelPickerOption } from './ModelPicker';
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
const EMPTY_MODELS: SessionModelOption[] = [];

function providerName(provider: 'claude' | 'codex'): string {
  return provider === 'claude' ? 'Claude' : 'Codex';
}

export function formatTokenEstimate(tokens: number): string {
  if (tokens < 1_000) return String(tokens);
  if (tokens < 10_000) return `${(tokens / 1_000).toFixed(1).replace(/\.0$/, '')}k`;
  return `${Math.round(tokens / 1_000)}k`;
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

function modelPickerOptions(models: SessionModelOption[]): ModelPickerOption[] {
  return models.map((model) => ({
    id: model.id,
    label: model.displayName || model.id,
    description: model.description || (model.isDefault ? 'Provider default' : model.id),
    isDefault: model.isDefault
  }));
}

function useProviderModels(): {
  catalogs: Partial<Record<'claude' | 'codex', SessionModelOption[]>>;
  errors: Partial<Record<'claude' | 'codex', string>>;
  loading: boolean;
} {
  const [catalogs, setCatalogs] = useState<Partial<Record<'claude' | 'codex', SessionModelOption[]>>>({});
  const [errors, setErrors] = useState<Partial<Record<'claude' | 'codex', string>>>({});
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    const controller = new AbortController();
    void fetchProviderStatuses(controller.signal, true).then((providers) => {
      if (controller.signal.aborted) return;
      const nextCatalogs: Partial<Record<'claude' | 'codex', SessionModelOption[]>> = {};
      const nextErrors: Partial<Record<'claude' | 'codex', string>> = {};
      for (const provider of providers) {
        nextCatalogs[provider.id] = (provider.models ?? []).filter((model) => !model.hidden);
        if (provider.modelsError) nextErrors[provider.id] = provider.modelsError;
      }
      setCatalogs(nextCatalogs);
      setErrors(nextErrors);
    }).catch((reason: unknown) => {
      if (!controller.signal.aborted) setErrors({ claude: String(reason), codex: String(reason) });
    }).finally(() => {
      if (!controller.signal.aborted) setLoading(false);
    });
    return () => controller.abort();
  }, []);
  return { catalogs, errors, loading };
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

function ContinuationPlan({
  selected,
  destination,
  model,
  effort,
  historyChoice,
  lastMessages,
  confirmed,
  models,
  modelError,
  modelsLoading,
  preview,
  previewLoading,
  previewError,
  disabled,
  onModel,
  onEffort,
  onHistoryChoice,
  onLastMessages,
  onConfirmed
}: {
  selected: ResumableSession;
  destination: 'claude' | 'codex';
  model: string;
  effort: string;
  historyChoice: HistoryChoice;
  lastMessages: number;
  confirmed: boolean;
  models: SessionModelOption[];
  modelError?: string;
  modelsLoading: boolean;
  preview: ContinuationPreview | null;
  previewLoading: boolean;
  previewError: string | null;
  disabled: boolean;
  onModel: (value: string) => void;
  onEffort: (value: string) => void;
  onHistoryChoice: (value: HistoryChoice) => void;
  onLastMessages: (value: number) => void;
  onConfirmed: (value: boolean) => void;
}): JSX.Element {
  const selectedModel = models.find((option) => option.id === model);
  const efforts = selectedModel?.supportedReasoningEfforts ?? [];
  const source = providerName(selected.tool);
  const destinationName = providerName(destination);
  const overThreshold = Boolean(preview && !preview.limited && preview.estimatedTokens > preview.thresholdTokens);
  return (
    <section className="continuation-plan">
      <div className="continuation-summary">
        <strong>{selected.title?.trim() || selected.firstUserMessage?.trim() || 'Saved conversation'}</strong>
        <span>{previewLoading ? 'Measuring the conversation…' : preview ? `${preview.messageCount} messages, about ${formatTokenEstimate(preview.estimatedTokens)} tokens` : 'Conversation size unavailable'}</span>
        <span>{destinationName} will start with <strong>{selectedModel?.displayName || model || 'a model you choose'}</strong>{effort ? ` · ${effort} effort` : ''}</span>
      </div>
      <div className="continuation-model-row">
        <label><span>Model</span><ModelPicker provider={destination} value={model} options={modelPickerOptions(models)} onChange={onModel} loading={modelsLoading} error={modelError} disabled={disabled} includeDefault={false} /></label>
        {efforts.length > 0 ? (
          <label><span>Effort</span><select value={effort} disabled={disabled} onChange={(event) => onEffort(event.currentTarget.value)}>
            {!selectedModel?.defaultReasoningEffort ? <option value="">Provider default</option> : null}
            {efforts.map((option) => <option value={option.reasoningEffort} key={option.reasoningEffort}>{option.reasoningEffort}</option>)}
          </select></label>
        ) : null}
      </div>
      <fieldset className="continuation-history-choice" disabled={disabled}>
        <legend>History to send</legend>
        <label><input type="radio" checked={historyChoice === 'all'} onChange={() => onHistoryChoice('all')} /> <span>The whole conversation</span></label>
        <label><input type="radio" checked={historyChoice === 'last'} onChange={() => onHistoryChoice('last')} /> <span>Only the last <input type="number" min="1" max={preview?.totalMessageCount || undefined} value={lastMessages} disabled={disabled || historyChoice !== 'last'} onChange={(event) => onLastMessages(Math.max(1, Number(event.currentTarget.value) || 1))} /> messages</span></label>
      </fieldset>
      {overThreshold ? (
        <label className="continuation-threshold"><input type="checkbox" checked={confirmed} disabled={disabled} onChange={(event) => onConfirmed(event.currentTarget.checked)} /><span><strong>Send the whole history anyway</strong><small>This is above the {formatTokenEstimate(preview?.thresholdTokens ?? 0)} token warning level.</small></span></label>
      ) : null}
      <div className="continuation-assurances">
        <span>Your {source} conversation is not changed</span>
        <span>Nothing is sent until you press Start</span>
      </div>
      {previewError ? <div className="dialog-error" role="alert">{previewError}</div> : null}
    </section>
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
  const [destination, setDestination] = useState<'claude' | 'codex'>(preferredDestinationProvider ?? selected?.tool ?? 'claude');
  const [runtimeMode, setRuntimeMode] = useState<'rich' | 'terminal'>(preferredRuntimeMode ?? (selected?.transcriptRecovery ? 'rich' : selected?.tool === 'claude' ? 'terminal' : 'rich'));
  const [historyChoice, setHistoryChoice] = useState<HistoryChoice>('all');
  const [lastMessages, setLastMessages] = useState(40);
  const [confirmed, setConfirmed] = useState(false);
  const [model, setModel] = useState('');
  const [effort, setEffort] = useState('');
  const { catalogs, errors: modelErrors, loading: modelsLoading } = useProviderModels();
  const models = catalogs[destination] ?? EMPTY_MODELS;
  const { preview, loading: previewLoading, error: previewError } = useContinuationPreview(selected, destination, historyChoice, lastMessages, preferredSourceSessionId);
  const crossProvider = Boolean(selected && selected.tool !== destination);
  useEffect(() => {
    const choice = models.find((option) => option.isDefault) ?? models[0];
    setModel(choice?.id ?? '');
    setEffort(choice?.defaultReasoningEffort ?? choice?.supportedReasoningEfforts[0]?.reasoningEffort ?? '');
  }, [destination, models]);
  const same = useSameProviderResume(selected, preferredSourceSessionId, destination, runtimeMode, refresh, onResumed, onClose);
  const continuation = useContinuationRun(refresh, onResumed, onClose);
  const working = crossProvider ? continuation.busy || continuation.job?.status === 'running' : same.busy;
  const error = crossProvider ? continuation.error : same.error;
  useEffect(() => onBusyChange(Boolean(working)), [onBusyChange, working]);
  const selectedModel = models.find((option) => option.id === model);
  const overThreshold = Boolean(preview && !preview.limited && preview.estimatedTokens > preview.thresholdTokens);
  const canStartCross = Boolean(preview && model && !previewError && !modelErrors[destination] && (!overThreshold || confirmed));
  const primaryText = !selected
    ? 'Choose a conversation'
    : working ? 'Starting…'
    : crossProvider
      ? preview && selectedModel
        ? `Start ${providerName(destination)} (${selectedModel.displayName || model}) with this history · ~${formatTokenEstimate(preview.estimatedTokens)} tokens`
        : 'Preparing details…'
      : `Continue ${providerName(destination)} conversation`;

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
      ) : selected && crossProvider ? (
        <ContinuationPlan
          selected={selected}
          destination={destination}
          model={model}
          effort={effort}
          historyChoice={historyChoice}
          lastMessages={lastMessages}
          confirmed={confirmed}
          models={models}
          modelError={modelErrors[destination]}
          modelsLoading={modelsLoading}
          preview={preview}
          previewLoading={previewLoading}
          previewError={previewError}
          disabled={working}
          onModel={(value) => {
            setModel(value);
            const choice = models.find((option) => option.id === value);
            setEffort(choice?.defaultReasoningEffort ?? choice?.supportedReasoningEfforts[0]?.reasoningEffort ?? '');
          }}
          onEffort={setEffort}
          onHistoryChoice={(choice) => { setHistoryChoice(choice); setConfirmed(false); }}
          onLastMessages={setLastMessages}
          onConfirmed={setConfirmed}
        />
      ) : selected ? (
        <div className="continuation-same-provider">
          Continues the original {providerName(selected.tool)} conversation. No copy is sent to another agent.
          {destination === 'claude' && runtimeMode === 'terminal' ? <small>Remote Control follows the explicit choice for the destination machine in Settings.</small> : null}
        </div>
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
        {selected && !continuation.job ? (
          <div className="resume-destination">
            <span>Continue with</span>
            <div role="radiogroup" aria-label="Agent for this conversation">
              {(['claude', 'codex'] as const).map((provider) => <button type="button" role="radio" aria-checked={destination === provider} className={destination === provider ? 'is-active' : ''} onClick={() => { setDestination(provider); setRuntimeMode(provider === selected.tool && !selected.transcriptRecovery && provider === 'claude' ? 'terminal' : 'rich'); }} disabled={working} key={provider}>{providerName(provider)}</button>)}
            </div>
          </div>
        ) : null}
        {!continuation.job ? <button type="button" className="btn btn-primary continuation-start" onClick={() => {
          if (!selected) return;
          if (crossProvider) void continuation.start({
            target: selected.sessionId, historyId: selected.historyId,
            sourceSessionId: sourceSessionID(selected, preferredSourceSessionId), destinationProvider: destination,
            model, effort, messageLimit: historyChoice === 'last' ? lastMessages : undefined,
            confirmWholeHistory: confirmed
          });
          else void same.resume();
        }} disabled={working || !selected || Boolean(same.partialResult) || (crossProvider && !canStartCross)}>{primaryText}</button> : null}
      </footer>
    </>
  );
}

function useSameProviderResume(
  selected: ResumableSession | null,
  sourceSessionId: string | undefined,
  destination: 'claude' | 'codex',
  runtimeMode: 'rich' | 'terminal',
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
      const outcome = await adoptConversationWithRepair(selected.sessionId, sourceSessionID(selected, sourceSessionId), selected.historyId, destination, runtimeMode);
      await refresh();
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
    void refresh().then(() => { onResumed(job.laneId as string); onClose(); }).catch((reason: unknown) => {
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
