// Render the parser's identity glyph. Upstream still produces emoji
// strings ("🟠" for Claude Code, "🟢" for Codex/OpenAI, "⬛" for
// unknown) — keeping that string-based contract avoids touching every
// place that produces or carries the icon. Here we just intercept
// the well-known emojis and swap them for real product art.

interface Props {
  icon: string;
  // Default 16px tracks the inline-text glyph size we replace; size up
  // for the StatusSidebar header and grid cells via the prop.
  size?: number;
  className?: string;
}

const IMG_BY_EMOJI: Record<string, { src: string; alt: string }> = {
  '🟠': { src: `${import.meta.env.BASE_URL}claude.png`,      alt: 'Claude' },
  '🟢': { src: `${import.meta.env.BASE_URL}openai-icon.svg`, alt: 'OpenAI' }
};

export function ParserIcon({ icon, size = 16, className }: Props): JSX.Element {
  const img = IMG_BY_EMOJI[icon];
  if (img) {
    return (
      <img
        src={img.src}
        alt={img.alt}
        width={size}
        height={size}
        className={className ? `parser-icon-img ${className}` : 'parser-icon-img'}
        // Inherit pixel dims via attrs but also lock CSS so the size
        // prop wins over any default img styling further down the tree.
        style={{ width: size, height: size }}
        draggable={false}
      />
    );
  }
  // Unknown / fallback emoji (e.g., "⬛") — render as a regular span so
  // the existing aria-hidden + size flows still apply.
  return <span aria-hidden className={className}>{icon}</span>;
}
