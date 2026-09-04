import type { ReactNode } from 'react';
import type { SessionModelOption } from '../api/sessionsd';
import type { PaidStartPlanState, PaidStartProvider } from '../hooks/usePaidStartPlan';
import { ModelPicker, type ModelPickerOption } from './ModelPicker';

interface Props {
  plan: PaidStartPlanState;
  title: string;
  sizeLine?: string;
  intro: string;
  sourceNote: string;
  copyNote?: string;
  disabled?: boolean;
  allowAgentChoice?: boolean;
  children?: ReactNode;
}

export function paidStartProviderName(provider: PaidStartProvider): string {
  return provider === 'claude' ? 'Claude' : 'Codex';
}

function modelOptions(models: SessionModelOption[]): ModelPickerOption[] {
  return models.map((model) => ({
    id: model.id,
    label: model.displayName || model.id,
    description: model.description || (model.isDefault ? 'Provider default' : model.id),
    isDefault: model.isDefault
  }));
}

function PlanChoice({ label, children }: { label: string; children: ReactNode }): JSX.Element {
  return <div className="paid-start-choice"><span>{label}</span>{children}</div>;
}

function AgentChoice({ plan, disabled }: { plan: PaidStartPlanState; disabled: boolean }): JSX.Element {
  return (
    <PlanChoice label="Agent">
      <div className="paid-start-segments" role="radiogroup" aria-label="Agent">
        {(['claude', 'codex'] as const).map((provider) => (
          <button type="button" role="radio" aria-checked={plan.destination === provider} className={plan.destination === provider ? 'is-active' : ''} disabled={disabled} onClick={() => plan.setDestination(provider)} key={provider}>
            {paidStartProviderName(provider)}
          </button>
        ))}
      </div>
    </PlanChoice>
  );
}

function RuntimeChoice({ plan, disabled }: { plan: PaidStartPlanState; disabled: boolean }): JSX.Element {
  return (
    <PlanChoice label="Runtime">
      <div className="paid-start-segments" role="radiogroup" aria-label="Runtime">
        <button type="button" role="radio" aria-checked={plan.runtime === 'rich'} className={plan.runtime === 'rich' ? 'is-active' : ''} disabled={disabled} onClick={() => plan.setRuntime('rich')}>Rich</button>
        <button type="button" role="radio" aria-checked={plan.runtime === 'terminal'} className={plan.runtime === 'terminal' ? 'is-active' : ''} disabled={disabled || !plan.terminalAvailable} title={plan.terminalAvailable ? undefined : 'Copied history starts in Rich'} onClick={() => plan.setRuntime('terminal')}>Terminal</button>
      </div>
    </PlanChoice>
  );
}

function ConfigurationRow({ plan, disabled, allowAgentChoice }: {
  plan: PaidStartPlanState;
  disabled: boolean;
  allowAgentChoice: boolean;
}): JSX.Element {
  const selectedModel = plan.models.find((entry) => entry.id === plan.model);
  const efforts = selectedModel?.supportedReasoningEfforts ?? [];
  return (
    <div className="paid-start-plan-row" role="group" aria-label="Start plan">
      {allowAgentChoice ? <AgentChoice plan={plan} disabled={disabled} /> : (
        <PlanChoice label="Agent"><strong>{paidStartProviderName(plan.destination)}</strong></PlanChoice>
      )}
      <PlanChoice label="Model">
        <ModelPicker provider={plan.destination} value={plan.model} options={modelOptions(plan.models)} onChange={plan.setModel} loading={plan.modelsLoading} error={plan.modelError} disabled={disabled} includeDefault={false} />
      </PlanChoice>
      <PlanChoice label="Effort">
        {efforts.length > 0 ? (
          <select aria-label="Effort" value={plan.effort} disabled={disabled} onChange={(event) => plan.setEffort(event.currentTarget.value)}>
            {!selectedModel?.defaultReasoningEffort ? <option value="">Provider default</option> : null}
            {efforts.map((option) => <option value={option.reasoningEffort} key={option.reasoningEffort}>{option.reasoningEffort}</option>)}
          </select>
        ) : <strong>{plan.effort || 'Provider default'}</strong>}
      </PlanChoice>
      <RuntimeChoice plan={plan} disabled={disabled} />
      <PlanChoice label="Access policy"><strong>{plan.access}</strong></PlanChoice>
    </div>
  );
}

export function PaidStartPlan({
  plan,
  title,
  sizeLine,
  intro,
  sourceNote,
  copyNote,
  disabled = false,
  allowAgentChoice = true,
  children
}: Props): JSX.Element {
  return (
    <section className="continuation-plan paid-start-plan">
      <div className="continuation-summary">
        <strong>{title}</strong>
        <span>{intro}</span>
        {sizeLine ? <span>{sizeLine}</span> : null}
      </div>
      <ConfigurationRow plan={plan} disabled={disabled} allowAgentChoice={allowAgentChoice} />
      {children}
      <div className="paid-start-boundary">
        <span>{sourceNote}</span>
        {copyNote ? <span>{copyNote}</span> : null}
        {plan.destination === 'claude' ? <span>Remote Control follows the explicit choice for the destination machine in Settings.</span> : null}
      </div>
      <div className="continuation-assurances">
        <span>Nothing runs until you press Start</span>
      </div>
      {plan.modelError ? <div className="dialog-error" role="alert">{plan.modelError}</div> : null}
    </section>
  );
}
