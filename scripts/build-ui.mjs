import { build } from 'esbuild';
import { ShieldCheck } from 'lucide';
import { writeFile } from 'node:fs/promises';
await build({entryPoints:['internal/guard/web/icons-src.js'],bundle:true,format:'iife',minify:true,outfile:'internal/guard/web/icons.js'});
const escape = v => String(v).replaceAll('&','&amp;').replaceAll('"','&quot;');
const shapes=ShieldCheck[2].map(([tag,attrs])=>`<${tag} ${Object.entries(attrs).map(([k,v])=>`${k}="${escape(v)}"`).join(' ')}/>`).join('');
await writeFile('internal/guard/web/logo.svg',`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="#72787c" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${shapes}</svg>`);
