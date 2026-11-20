// render.mjs: frames JSON (from `go run ./tools/facesvg`) on stdin -> an animated SVG of
// the clock face on stdout. The segment shapes come from the web UI's own face
// picture (FACE in internal/web/ui/index.html), so the README shows the same face.
// Pure CSS animation (no script), which GitHub allows in README images.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const ui = readFileSync(join(here, '../../internal/web/ui/index.html'), 'utf8');
const start = ui.indexOf('const FACE = (() => {');
const end = ui.indexOf('})();', start) + '})();'.length;
if (start < 0 || end < start) throw new Error('FACE not found in index.html');
const FACE = new Function(ui.slice(start, end) + '\nreturn FACE;')();
// the icons are <use href="#i-…"> references to the page's sprite: copy those symbols in
const symbols = [...new Set(FACE.flatMap(p => [...p[3].matchAll(/href="#(i-[\w-]+)"/g)].map(m => m[1])))].map(id => {
  const m = ui.match(new RegExp(`<symbol id="${id}"[\\s\\S]*?</symbol>`));
  if (!m) throw new Error(`symbol ${id} not found in index.html`);
  return m[0];
});

const frames = JSON.parse(readFileSync(0, 'utf8'));
const total = frames.reduce((s, f) => s + f.ms, 0);
const lit = (f, [chip, grid, mask]) => (((chip === 'a' ? f.a : f.b)[grid] || 0) & mask) === mask;

// one keyframes rule per on/off pattern; step-end holds each value until the next stop
const patterns = new Map();
const parts = FACE.map((p, i) => {
  const seq = frames.map(f => (lit(f, p) ? 1 : 0));
  if (!seq.some(Boolean)) return `<g class="sg">${p[3]}</g>`;
  const key = seq.join('');
  if (!patterns.has(key)) patterns.set(key, `k${patterns.size}`);
  return `<g class="sg ${patterns.get(key)}">${p[3]}</g>`;
});
const pct = ms => +(100 * ms / total).toFixed(3);
const rules = [...patterns].map(([key, name]) => {
  let t = 0, prev = null, stops = [];
  key.split('').forEach((v, i) => {
    if (v !== prev) stops.push(`${pct(t)}%{opacity:${v === '1' ? 1 : 0.03}}`);
    prev = v; t += frames[i].ms;
  });
  stops.push(`100%{opacity:${prev === '1' ? 1 : 0.03}}`);
  return `@keyframes ${name}{${stops.join('')}}.${name}{animation:${name} ${total / 1000}s step-end infinite}`;
});

process.stdout.write(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="-20 -20 1040 574" width="520" role="img" aria-label="The clock face: the time 21:47 with a blinking colon, Thursday, snow and -3 °C, then the scrolling text hello luna">
<style>
.sg{fill:#f3f6fa;stroke:#f3f6fa;opacity:.03}.sg polygon{stroke:none}
text{stroke:none;font:600 33px system-ui,-apple-system,Segoe UI,Roboto,sans-serif}text.ft{font-size:31px}
.ico,.ln{fill:none!important;stroke-width:2.4;stroke-linecap:round;stroke-linejoin:round}.ln{stroke-width:6}.ln circle{stroke-width:3.5}.ln.fog{stroke-width:4.5}
${rules.join('\n')}
</style>
<defs>
${symbols.join('\n')}
<linearGradient id="fb" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#26282c"/><stop offset=".08" stop-color="#141518"/><stop offset="1" stop-color="#0a0a0c"/></linearGradient>
<filter id="glow" x="-50%" y="-50%" width="200%" height="200%"><feGaussianBlur stdDeviation="2.5" result="b"/><feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge></filter>
</defs>
<rect x="0" y="0" width="1000" height="534" rx="155" fill="url(#fb)" stroke="#33363c" stroke-width="3"/>
<path d="M150 14h700" stroke="#fff" stroke-opacity=".10" stroke-width="5" stroke-linecap="round"/>
<g fill="#1d1f23">${[0, 1, 2, 3].map(i => `<circle cx="${440 + i * 40}" cy="62" r="7"/>`).join('')}</g>
<g filter="url(#glow)">${parts.join('')}</g>
</svg>
`);
