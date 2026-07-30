import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { ProviderMark } from './ProviderBadge';

export interface ModelPickerOption {
  id: string;
  label: string;
  description?: string;
  isDefault?: boolean;
}

interface Props {
  provider: 'claude' | 'codex';
  value: string;
  options: ModelPickerOption[];
  onChange: (value: string) => void;
  loading?: boolean;
  error?: string | null;
  disabled?: boolean;
  includeDefault?: boolean;
  defaultLabel?: string;
  compact?: boolean;
  allowCustom?: boolean;
}

export const CLAUDE_MODEL_OPTIONS: ModelPickerOption[] = [
  { id: 'claude-fable-5', label: 'Fable 5', description: 'Fast, capable everyday work' },
  { id: 'opus', label: 'Opus (alias)', description: 'Deep reasoning; follows the Claude CLI alias' },
  { id: 'sonnet', label: 'Sonnet', description: 'Balanced speed and reasoning' },
  { id: 'haiku', label: 'Haiku', description: 'Fast, lightweight tasks' }
];

export function ModelPicker({
  provider,
  value,
  options,
  onChange,
  loading = false,
  error = null,
  disabled = false,
  includeDefault = true,
  defaultLabel = 'Default',
  compact = false,
  allowCustom = false
}: Props): JSX.Element {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [activeIndex, setActiveIndex] = useState(0);
  const listId = useId();
  const rootRef = useRef<HTMLDivElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const providerName = provider === 'claude' ? 'Claude' : 'Codex';
  const selected = options.find((option) => option.id === value);
  const defaultVisible = includeDefault && !query.trim();
  const customValue = allowCustom
    && query.trim()
    && !options.some((option) => option.id.toLowerCase() === query.trim().toLowerCase())
    ? query.trim()
    : '';

  const orderedOptions = useMemo(() => {
    const lowered = query.trim().toLowerCase();
    const visible = options.filter((option) => {
      if (!lowered) return true;
      return `${option.label} ${option.id} ${option.description ?? ''}`.toLowerCase().includes(lowered);
    });
    return visible.sort((left, right) => {
      if (left.isDefault !== right.isDefault) return left.isDefault ? -1 : 1;
      return options.indexOf(left) - options.indexOf(right);
    });
  }, [options, query]);

  useEffect(() => {
    if (!open) return;
    setQuery('');
    setActiveIndex(0);
    const focus = window.setTimeout(() => searchRef.current?.focus(), 0);
    const close = (event: PointerEvent): void => {
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
    };
    window.addEventListener('pointerdown', close);
    return () => {
      window.clearTimeout(focus);
      window.removeEventListener('pointerdown', close);
    };
  }, [open]);

  useEffect(() => {
    const itemCount = orderedOptions.length + (defaultVisible ? 1 : 0) + (customValue ? 1 : 0);
    setActiveIndex((current) => Math.min(current, Math.max(0, itemCount - 1)));
  }, [customValue, defaultVisible, orderedOptions.length]);

  const choose = (next: string): void => {
    onChange(next);
    setOpen(false);
  };
  const activeOptionId = `${listId}-option-${activeIndex}`;

  const onKeyDown = (event: KeyboardEvent): void => {
    if (event.key === 'Escape') {
      event.preventDefault();
      setOpen(false);
      return;
    }
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      const itemCount = orderedOptions.length + (defaultVisible ? 1 : 0) + (customValue ? 1 : 0);
      setActiveIndex((current) => Math.min(current + 1, itemCount - 1));
      return;
    }
    if (event.key === 'ArrowUp') {
      event.preventDefault();
      setActiveIndex((current) => Math.max(0, current - 1));
      return;
    }
    if (event.key === 'Enter' && defaultVisible && activeIndex === 0) {
      event.preventDefault();
      choose('');
      return;
    }
    const activeOption = orderedOptions[activeIndex - (defaultVisible ? 1 : 0)];
    if (event.key === 'Enter' && activeOption) {
      event.preventDefault();
      choose(activeOption.id);
      return;
    }
    if (event.key === 'Enter' && customValue && activeIndex === orderedOptions.length + (defaultVisible ? 1 : 0)) {
      event.preventDefault();
      choose(customValue);
      return;
    }
    if ((event.metaKey || event.ctrlKey) && /^[1-9]$/.test(event.key)) {
      const indexed = orderedOptions[Number(event.key) - 1];
      if (indexed) {
        event.preventDefault();
        choose(indexed.id);
      }
    }
  };

  return (
    <div className={`model-picker${compact ? ' is-compact' : ''}`} ref={rootRef}>
      <button
        type="button"
        className="model-picker-trigger"
        onClick={() => setOpen((current) => !current)}
        aria-expanded={open}
        aria-haspopup="listbox"
        aria-controls={open ? listId : undefined}
        disabled={disabled}
      >
        <ProviderMark provider={provider} size={compact ? 16 : 18} />
        <span>{value ? selected?.label ?? value : defaultLabel}</span>
        <span className="model-picker-chevron" aria-hidden>⌄</span>
      </button>
      {open ? (
        <section className="model-picker-popover" onKeyDown={onKeyDown} aria-label={`${providerName} model picker`}>
          <header>
            <span><ProviderMark provider={provider} size={20} /><strong>{providerName} models</strong></span>
            <button type="button" onClick={() => setOpen(false)} aria-label="Close model picker">×</button>
          </header>
          <div className="model-picker-search">
            <span aria-hidden>⌕</span>
            <input
              ref={searchRef}
              value={query}
              onChange={(event) => {
                setQuery(event.currentTarget.value);
                setActiveIndex(0);
              }}
              placeholder={`Search ${providerName} models`}
              aria-label={`Search ${providerName} models`}
              maxLength={128}
              role="combobox"
              aria-autocomplete="list"
              aria-expanded="true"
              aria-controls={listId}
              aria-activedescendant={activeOptionId}
            />
          </div>
          <div id={listId} className="model-picker-list" role="listbox" aria-label={`${providerName} models`}>
            {defaultVisible ? (
              <button
                id={`${listId}-option-0`}
                type="button"
                role="option"
                aria-selected={value === ''}
                className={`model-picker-option${value === '' ? ' is-selected' : ''}${activeIndex === 0 ? ' is-focused' : ''}`}
                onMouseEnter={() => setActiveIndex(0)}
                onClick={() => choose('')}
              >
                <span className="model-picker-option-copy"><strong>{defaultLabel}</strong><small>Use your {providerName} setting</small></span>
                {value === '' ? <span className="model-picker-check" aria-hidden>✓</span> : null}
              </button>
            ) : null}
            {orderedOptions.map((option, index) => {
              const optionIndex = index + (defaultVisible ? 1 : 0);
              return (
                <button
                  id={`${listId}-option-${optionIndex}`}
                  key={option.id}
                  type="button"
                  role="option"
                  aria-selected={value === option.id}
                  className={`model-picker-option${value === option.id ? ' is-selected' : ''}${optionIndex === activeIndex ? ' is-focused' : ''}`}
                  onMouseEnter={() => setActiveIndex(optionIndex)}
                  onClick={() => choose(option.id)}
                >
                  <span className="model-picker-option-copy">
                    <strong>{option.label}</strong>
                    <small>{option.description || option.id}</small>
                  </span>
                  <span className="model-picker-option-actions">
                    {index < 9 ? <kbd>⌘{index + 1}</kbd> : null}
                    {value === option.id ? <span className="model-picker-check" aria-hidden>✓</span> : null}
                  </span>
                </button>
              );
            })}
            {customValue ? (
              <button
                id={`${listId}-option-${orderedOptions.length + (defaultVisible ? 1 : 0)}`}
                type="button"
                role="option"
                aria-selected={value === customValue}
                className={`model-picker-option is-custom${activeIndex === orderedOptions.length + (defaultVisible ? 1 : 0) ? ' is-focused' : ''}`}
                onMouseEnter={() => setActiveIndex(orderedOptions.length + (defaultVisible ? 1 : 0))}
                onClick={() => choose(customValue)}
              >
                <span className="model-picker-option-copy"><strong>Use exact model ID</strong><small>{customValue}</small></span>
                <span className="model-picker-check" aria-hidden>↵</span>
              </button>
            ) : null}
            {!loading && orderedOptions.length === 0 ? (
              <div className="model-picker-empty">No matching models</div>
            ) : null}
          </div>
          <footer>
            {loading ? 'Loading the live catalog…' : error ? 'Live catalog unavailable; provider defaults still work.' : '↑↓ navigate · Enter selects'}
          </footer>
        </section>
      ) : null}
    </div>
  );
}
