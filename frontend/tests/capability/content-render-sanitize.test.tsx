// CAPABILITY: provider text rendered as HTML cannot run script.
//
// RemoteView and GridView hand renderContent's output to
// dangerouslySetInnerHTML, so this pipeline is the only thing between a
// provider transcript and the app origin (and the Tauri WebView). The Node
// markdown smoke runs without a DOM and therefore skips the DOMPurify pass;
// this suite runs in a DOM so the complete browser pipeline, including the
// final allow-list sanitizer, is what gets measured.
//
// ansiToHtml passes `<`, `>` and `&` through unescaped, so the DOMPurify
// allow-list is the boundary, not a second layer. It runs in string mode with
// no hooks, so the IN_PLACE advisories fixed in dompurify 3.4.16
// (GHSA-p98j-92pf-mc4p, GHSA-6688-9rhm-gjv2) did not reach this path. These
// tests pin the boundary itself rather than a package version.
import { describe, expect, it } from 'vitest';
import { renderContent } from '../../src/lib/contentRender';

function mount(html: string): HTMLElement {
  const host = document.createElement('div');
  host.innerHTML = html;
  return host;
}

function scriptableSurface(host: HTMLElement): string[] {
  const found: string[] = [];
  for (const element of Array.from(host.querySelectorAll('*'))) {
    const tag = element.tagName.toLowerCase();
    if (['script', 'iframe', 'object', 'embed', 'img', 'svg', 'math', 'style', 'form'].includes(tag)) found.push(`<${tag}>`);
    for (const attribute of Array.from(element.attributes)) {
      if (/^on/i.test(attribute.name)) found.push(`${tag}[${attribute.name}]`);
      if (attribute.name === 'href' && /^\s*(javascript|data|vbscript):/i.test(attribute.value)) found.push(`${tag}[href=${attribute.value}]`);
    }
  }
  return found;
}

describe('capability: rendered provider text stays inert', () => {
  it('never turns injected markup into a scriptable element or handler', () => {
    // ansiToHtml does not entity-escape, so raw markup reaches marked and the
    // DOM sanitizer is the boundary that removes it.
    const payloads = [
      '<img src=x onerror="window.__pwned=1">',
      '<script>window.__pwned=1</script>',
      '<svg><animate onbegin="window.__pwned=1" attributeName=x /></svg>',
      '<style></style><img src=x onerror=alert(1)>',
      '<iframe srcdoc="<script>parent.__pwned=1</script>"></iframe>',
      '<math><mtext><table><mglyph><style><img src=x onerror=alert(1)>'
    ];
    for (const payload of payloads) {
      const host = mount(renderContent(payload));
      expect(scriptableSurface(host), payload).toEqual([]);
    }
  });

  it('neutralizes executable link destinations from markdown', () => {
    for (const markdown of [
      '[a](javascript:alert(1))',
      '[b](JaVaScRiPt:alert(1))',
      '[c](data:text/html,<script>alert(1)</script>)',
      '[d](vbscript:msgbox(1))'
    ]) {
      const host = mount(renderContent(markdown));
      expect(scriptableSurface(host), markdown).toEqual([]);
    }
  });

  it('applies the final allow-list in a DOM, removing tags the earlier stages emit but the view never uses', () => {
    // marked turns image syntax into <img>; only the DOM sanitizer stage can
    // remove it, so this proves that stage ran.
    const host = mount(renderContent('![tracker](https://example.invalid/pixel.png)'));
    expect(host.querySelector('img')).toBeNull();
  });

  it('keeps the markup the view relies on', () => {
    const host = mount(renderContent('\u001b[31mred\u001b[0m and [docs](https://example.com)\n\n```\ncode\n```'));
    expect(host.querySelector('span[style]')?.textContent).toBe('red');
    expect(host.querySelector('a[href="https://example.com"]')?.textContent).toBe('docs');
    const copy = host.querySelector('button[data-code-copy]');
    expect(copy).not.toBeNull();
    expect(copy?.getAttribute('type')).toBe('button');
    expect(host.querySelector('pre code')?.textContent).toBe('code\n');
  });
});
