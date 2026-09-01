import type { DispatchMessage } from '../hooks/useDispatch';

export function PlanPanel({
  steps,
  explanation
}: {
  steps: NonNullable<DispatchMessage['plan']>;
  explanation?: string;
}): JSX.Element {
  const done = steps.filter((step) => step.status.toLowerCase() === 'completed').length;
  return (
    <details
      className="remote-bubble-plan"
      open={done < steps.length}
      data-no-copy
      onClick={(event) => event.stopPropagation()}
    >
      <summary>Plan · {done}/{steps.length}</summary>
      {explanation ? <p className="remote-bubble-plan-explanation">{explanation}</p> : null}
      <div className="remote-bubble-plan-steps">
        {steps.map((step, index) => {
          const status = step.status.toLowerCase();
          const marker = status === 'completed' ? '✓' : status === 'inprogress' || status === 'in_progress' ? '●' : '○';
          return (
            <div key={`${index}-${step.step}`} className={`remote-bubble-plan-step is-${status}`}>
              <span aria-hidden>{marker}</span>
              <span>{step.step}</span>
            </div>
          );
        })}
      </div>
    </details>
  );
}
