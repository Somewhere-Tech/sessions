import { useEffect, useMemo, useState } from 'react';
import { fetchProviderStatuses, type SessionModelOption } from '../api/sessionsd';

export type PaidStartProvider = 'claude' | 'codex';
export type PaidStartRuntime = 'rich' | 'terminal';

interface Options {
  sourceProvider: PaidStartProvider;
  preferredDestination?: PaidStartProvider;
  preferredRuntime?: PaidStartRuntime;
  terminalAvailable: boolean;
}

export interface PaidStartPlanState {
  destination: PaidStartProvider;
  model: string;
  modelName: string;
  effort: string;
  runtime: PaidStartRuntime;
  access: 'Ask me';
  models: SessionModelOption[];
  modelError: string | null;
  modelsLoading: boolean;
  terminalAvailable: boolean;
  ready: boolean;
  setDestination: (provider: PaidStartProvider) => void;
  setModel: (model: string) => void;
  setEffort: (effort: string) => void;
  setRuntime: (runtime: PaidStartRuntime) => void;
}

const EMPTY_MODELS: SessionModelOption[] = [];

export function formatTokenEstimate(tokens: number): string {
  if (tokens < 1_000) return String(tokens);
  if (tokens < 10_000) return `${(tokens / 1_000).toFixed(1).replace(/\.0$/, '')}k`;
  return `${Math.round(tokens / 1_000)}k`;
}

export function usePaidStartPlan({
  sourceProvider,
  preferredDestination,
  preferredRuntime,
  terminalAvailable
}: Options): PaidStartPlanState {
  const [destination, setDestination] = useState<PaidStartProvider>(preferredDestination ?? sourceProvider);
  const [runtime, setRuntime] = useState<PaidStartRuntime>(preferredRuntime ?? 'rich');
  const [model, setModelState] = useState('');
  const [effort, setEffort] = useState('');
  const [catalogs, setCatalogs] = useState<Partial<Record<PaidStartProvider, SessionModelOption[]>>>({});
  const [errors, setErrors] = useState<Partial<Record<PaidStartProvider, string>>>({});
  const [modelsLoading, setModelsLoading] = useState(true);
  const canUseTerminal = terminalAvailable && destination === sourceProvider;

  useEffect(() => {
    const controller = new AbortController();
    void fetchProviderStatuses(controller.signal, true).then((providers) => {
      if (controller.signal.aborted) return;
      const nextCatalogs: Partial<Record<PaidStartProvider, SessionModelOption[]>> = {};
      const nextErrors: Partial<Record<PaidStartProvider, string>> = {};
      for (const provider of providers) {
        nextCatalogs[provider.id] = (provider.models ?? []).filter((entry) => !entry.hidden);
        if (provider.modelsError) nextErrors[provider.id] = provider.modelsError;
      }
      setCatalogs(nextCatalogs);
      setErrors(nextErrors);
    }).catch((reason: unknown) => {
      if (!controller.signal.aborted) {
        const message = reason instanceof Error ? reason.message : 'Could not load model choices.';
        setErrors({ claude: message, codex: message });
      }
    }).finally(() => {
      if (!controller.signal.aborted) setModelsLoading(false);
    });
    return () => controller.abort();
  }, []);

  const models = catalogs[destination] ?? EMPTY_MODELS;
  useEffect(() => {
    const choice = models.find((entry) => entry.isDefault) ?? models[0];
    setModelState(choice?.id ?? '');
    setEffort(choice?.defaultReasoningEffort ?? choice?.supportedReasoningEfforts[0]?.reasoningEffort ?? '');
  }, [destination, models]);
  useEffect(() => {
    if (!canUseTerminal && runtime !== 'rich') setRuntime('rich');
  }, [canUseTerminal, runtime]);

  const selectedModel = useMemo(() => models.find((entry) => entry.id === model), [model, models]);
  const modelError = errors[destination] ?? (!modelsLoading && models.length === 0 ? `No ${destination === 'claude' ? 'Claude' : 'Codex'} models are available.` : null);
  const setModel = (value: string): void => {
    setModelState(value);
    const choice = models.find((entry) => entry.id === value);
    setEffort(choice?.defaultReasoningEffort ?? choice?.supportedReasoningEfforts[0]?.reasoningEffort ?? '');
  };

  return {
    destination,
    model,
    modelName: selectedModel?.displayName || model,
    effort,
    runtime: canUseTerminal ? runtime : 'rich',
    access: 'Ask me',
    models,
    modelError,
    modelsLoading,
    terminalAvailable: canUseTerminal,
    ready: Boolean(selectedModel && !modelsLoading && !modelError),
    setDestination,
    setModel,
    setEffort,
    setRuntime
  };
}
