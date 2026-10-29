/**
 * Frozen, DOM-based dimension reconciliation. No worker metadata is consulted.
 *
 * page.evaluate(collectDrawingFacts) -> {version, tables, texts, units, limitations}.
 * checkDrawingFacts(observation, sourceFacts) -> {status, findings, counts}.
 * sourceFacts is supplied ONLY by the independent evaluator:
 *   {values: {S1: '14 px', V1: null, W: '420–680 px'}, defaultUnit: 'px'}
 * Values are strings in the same restricted arithmetic grammar as table values;
 * null/absent means the source supplies no value. defaultUnit applies to bare
 * lengths, never to multiplication factors. It is independent of page units.
 * PASS proves reconciliation of collected text, not visual quality or source
 * completeness. Canvas, outlined text, cross-frame content, unsupported tables,
 * contextual symbols (e.g. h), mixed relative units, correlated ranges and unknown source facts
 * cannot be inferred. These produce UNKNOWN when detected/needed. SVG tables
 * require adjacent ruled cells or an outlined straight-line grid. Range separators
 * are en/em dash or ".."; ASCII "-" always means arithmetic subtraction.
 * Absolute CSS lengths use 96 px/in. Relative units are never guessed from
 * the evaluator browser viewport. Missing source entries are unaudited, not proof.
 */
export function collectDrawingFacts() {
  // Keep every helper inside this function: Playwright serializes it by value.
  const clean = s => String(s || '').replace(/\s+/g, ' ').trim();
  const tables = [], texts = [], units = [], limitations = [];
  const consumed = new Set();
  const visible = el => {
    for (let p = el; p && p.nodeType === 1; p = p.parentElement) {
      const s = getComputedStyle(p);
      if (p.hidden || s.display === 'none' || s.visibility === 'hidden' || s.opacity === '0') return false;
    }
    return true;
  };
  const location = el => {
    const parts = [];
    for (let p = el; p && p !== document.body; p = p.parentElement) {
      parts.unshift(`${p.localName}:nth-child(${[...p.parentElement?.children || []].indexOf(p) + 1})`);
    }
    return 'body > ' + parts.join(' > ');
  };
  const region = el => el.closest('header, .tb') ? 'header'
    : el.closest('footer, .foot') ? 'footer' : el.closest('svg') ? 'drawing' : 'body';
  const add = (el, text = el.textContent, extra = {}) => {
    if (visible(el) && clean(text)) texts.push({text: clean(text), region: region(el), location: location(el), ...extra});
  };
  const headerName = s => clean(s).toLowerCase();
  const columnIndexes = cells => ['code', 'variable', 'value'].map(h => cells.findIndex(c => headerName(c.textContent) === h));
  for (const table of document.querySelectorAll('table, [role="table"]')) {
    if (!visible(table)) continue;
    const rows = [...table.querySelectorAll('tr, [role="row"]')].filter(r => r.closest('table, [role="table"]') === table);
    const cells = r => [...r.querySelectorAll('th, td, [role="cell"], [role="columnheader"]')];
    const head = rows.findIndex(r => columnIndexes(cells(r)).every(i => i >= 0));
    if (head < 0) { limitations.push({kind: 'unrecognized-table', location: location(table)}); continue; }
    const indexes = columnIndexes(cells(rows[head]));
    const result = {location: location(table), rows: []};
    for (const r of rows.slice(head + 1).filter(visible)) {
      const cs = cells(r), selected = indexes.map(i => cs[i]);
      if (selected.some(c => !c || !visible(c)) || cs.some(c => c.colSpan > 1 || c.rowSpan > 1)) {
        limitations.push({kind: 'ambiguous-table-row', location: location(r)}); continue;
      }
      result.rows.push({code: clean(selected[0].textContent), variable: clean(selected[1].textContent), value: clean(selected[2].textContent), location: location(r)});
    }
    table.querySelectorAll('*').forEach(el => consumed.add(el));
    consumed.add(table);
    tables.push(result);
  }
  // The drawing kit uses three individually ruled cells per row. Match headers
  // to their actual cells and follow contiguous rows; do not scrape capital words.
  for (const svg of document.querySelectorAll('svg')) {
    if (!visible(svg)) continue;
    const ts = [...svg.querySelectorAll('text')].filter(visible);
    const point = (el, x, y) => {
      const m = el.getCTM?.();
      if (m) return {x: m.a*x + m.c*y + m.e, y: m.b*x + m.d*y + m.f};
      if (el.closest('[transform]')) return null;
      return {x, y};
    };
    const num = (el, a) => Number.parseFloat(el.getAttribute(a) || '0');
    const rects = [...svg.querySelectorAll('rect')].map(el => {
      const x = num(el, 'x'), y = num(el, 'y'), w = num(el, 'width'), h = num(el, 'height');
      const ps = [[x,y],[x+w,y],[x,y+h],[x+w,y+h]].map(([a,b]) => point(el,a,b));
      if (ps.some(p => !p) || !visible(el)) return null;
      return {el, x: Math.min(...ps.map(p=>p.x)), y: Math.min(...ps.map(p=>p.y)),
        right: Math.max(...ps.map(p=>p.x)), bottom: Math.max(...ps.map(p=>p.y))};
    }).filter(Boolean);
    // A common second kit draws one outline and a path grid, rather than one
    // rectangle per cell. Recover cells only from full-length visible rules.
    const segments = [];
    for (const el of svg.querySelectorAll('line,path')) {
      if (!visible(el)) continue;
      if (el.localName === 'line') {
        segments.push([point(el,num(el,'x1'),num(el,'y1')),point(el,num(el,'x2'),num(el,'y2'))]);
        continue;
      }
      const raw=el.getAttribute('d') || '';
      if (raw.replace(/[-+]?\d*\.?\d+(?:e[-+]?\d+)?/gi,'').replace(/[MLHVZmlhvz,\s]/g,'')) continue;
      const tokens=raw.match(/[a-zA-Z]|[-+]?(?:\d*\.\d+|\d+\.?\d*)(?:e[-+]?\d+)?/g) || [];
      let i=0,x=0,y=0,sx=0,sy=0,command='';
      while (i<tokens.length) {
        if (/^[a-z]$/i.test(tokens[i])) command=tokens[i++];
        const relative=command===command.toLowerCase(), op=command.toUpperCase();
        let nx=x,ny=y;
        if (op==='Z') { nx=sx;ny=sy;command=''; }
        else if (op==='M' || op==='L') {
          if (i+1>=tokens.length || /^[a-z]$/i.test(tokens[i]) || /^[a-z]$/i.test(tokens[i+1])) break;
          nx=Number(tokens[i++])+(relative?x:0); ny=Number(tokens[i++])+(relative?y:0);
        } else if (op==='H' || op==='V') {
          if (i>=tokens.length || /^[a-z]$/i.test(tokens[i])) break;
          const v=Number(tokens[i++]); if(op==='H') nx=v+(relative?x:0); else ny=v+(relative?y:0);
        } else break;
        if (op==='M') {sx=nx;sy=ny;command=relative?'l':'L';}
        else segments.push([point(el,x,y),point(el,nx,ny)]);
        x=nx;y=ny;
      }
    }
    const nearGrid=(a,b)=>Math.abs(a-b)<.75;
    const outlines=[...rects];
    for (const outline of outlines) {
      const hs=ts.filter(t=>['code','variable','value'].includes(headerName(t.textContent)) && (()=>{
        const p=point(t,num(t,'x'),num(t,'y'));return p && p.x>=outline.x && p.x<=outline.right && p.y>outline.y && p.y<=outline.bottom;
      })());
      if (!['code','variable','value'].every(name=>hs.some(t=>headerName(t.textContent)===name))) continue;
      const xs=[outline.x,outline.right],ys=[outline.y,outline.bottom];
      for (const [a,b] of segments) {
        if (!a || !b) continue;
        if (nearGrid(a.x,b.x) && nearGrid(Math.min(a.y,b.y),outline.y) && nearGrid(Math.max(a.y,b.y),outline.bottom) && a.x>outline.x && a.x<outline.right) xs.push(a.x);
        if (nearGrid(a.y,b.y) && nearGrid(Math.min(a.x,b.x),outline.x) && nearGrid(Math.max(a.x,b.x),outline.right) && a.y>outline.y && a.y<outline.bottom) ys.push(a.y);
      }
      const unique=values=>values.sort((a,b)=>a-b).filter((v,i,all)=>!i || !nearGrid(v,all[i-1]));
      const columns=unique(xs),rows=unique(ys);
      if(columns.length!==4 || rows.length<3) continue;
      for(let r=0;r<rows.length-1;r++) for(let c=0;c<3;c++) rects.push({el:outline.el,
        x:columns[c],right:columns[c+1],y:rows[r],bottom:rows[r+1]});
    }
    const anchors = new Map(ts.map(el => [el, point(el, num(el,'x'), num(el,'y'))]));
    const inside = (r, p) => p && p.x >= r.x && p.x <= r.right && p.y > r.y && p.y <= r.bottom;
    const cellFor = el => rects.filter(r => inside(r, anchors.get(el)))
      .sort((a,b) => (a.right-a.x)*(a.bottom-a.y)-(b.right-b.x)*(b.bottom-b.y))[0];
    const near = (a,b) => Math.abs(a-b) < .75;
    for (const codeHead of ts.filter(t => headerName(t.textContent) === 'code')) {
      if (consumed.has(codeHead)) continue;
      const c = cellFor(codeHead);
      const heads = c && ['variable','value'].map((name,i) => ts.find(t => {
        const r = cellFor(t);
        return headerName(t.textContent) === name && r && near(r.y,c.y) && near(r.bottom,c.bottom)
          && (i === 0 ? near(r.x,c.right) : r.x > c.right);
      }));
      const hs = heads && [codeHead, ...heads];
      if (!hs || hs.some(h => !h)) { limitations.push({kind:'unrecognized-svg-table', location:location(codeHead)}); continue; }
      const columns = hs.map(cellFor);
      if (!near(columns[1].right,columns[2].x)) { limitations.push({kind:'unrecognized-svg-table', location:location(codeHead)}); continue; }
      hs.forEach(h => consumed.add(h));
      const result = {location: location(codeHead), rows: []};
      let y = c.bottom;
      for (let n = 0; n < ts.length; n++) {
        const row = columns.map(col => rects.find(r => near(r.x,col.x) && near(r.right,col.right) && near(r.y,y)));
        if (row.every(r => !r)) break;
        if (row.some(r => !r) || !row.every(r => near(r.bottom,row[0].bottom))) {
          limitations.push({kind:'ambiguous-svg-row', location:location(codeHead), y}); break;
        }
        const cells = row.map(r => ts.filter(t => inside(r,anchors.get(t))));
        if (cells.some(cs => cs.length !== 1)) {
          limitations.push({kind:'ambiguous-svg-cell', location:location(codeHead), y}); break;
        }
        const els = cells.flat();
        if (els.every((el,i) => headerName(el.textContent) === ['code','variable','value'][i])) break;
        els.forEach(el => consumed.add(el));
        result.rows.push({code:clean(els[0].textContent), variable:clean(els[1].textContent), value:clean(els[2].textContent), location:location(els[0])});
        y = row[0].bottom;
      }
      tables.push(result);
    }
    for (const t of ts) if (!consumed.has(t)) {
      add(t, t.textContent, {heading: t.matches('.vt, .vtb, .sl, .nth, .tbk, .tbt')});
      if (/^(?:CSS\s+PX|mm|cm|in|pt)$/i.test(clean(t.textContent))) units.push({text:clean(t.textContent),location:location(t)});
    }
    if (svg.querySelector('use, foreignObject, image')) limitations.push({kind:'embedded-svg-content', location:location(svg)});
  }
  // Summary cards are uses/claims, never additional table definitions.
  for (const card of document.querySelectorAll('.figs > div')) {
    const label = card.querySelector('.lbl'), value = card.querySelector('.v');
    if (label && value) {
      const m = clean(label.textContent).match(/^([A-Z][A-Z0-9]*)\s*[·:]/);
      if (m) {
        add(card, `${m[1]} = ${clean(value.textContent)}`);
        card.querySelectorAll('*').forEach(el => consumed.add(el)); consumed.add(card);
      }
    }
  }
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  const seen = new Set();
  while (walker.nextNode()) {
    if (!clean(walker.currentNode.textContent)) continue;
    const leaf = walker.currentNode.parentElement;
    if (!leaf || consumed.has(leaf) || leaf.closest('svg, script, style, template, noscript')) continue;
    if (/^units$/i.test(clean(leaf.textContent))) {
      const value = clean(leaf.nextElementSibling?.textContent);
      if (value && visible(leaf)) units.push({text:value, location:location(leaf)});
    }
    // Join inline spans only within a leaf block, never across table/title cells.
    const block = leaf.closest('p,li,footer,.foot,div,header');
    const el = block && !consumed.has(block) && !block.querySelector('p,li,div,table,svg,h1,h2,h3,h4,h5,h6') ? block : leaf;
    const ownOnly=el===leaf && el.querySelector('p,li,div,table,svg,h1,h2,h3,h4,h5,h6');
    const key=ownOnly ? walker.currentNode : el;
    if (seen.has(key)) continue;
    seen.add(key);
    add(el, ownOnly ? walker.currentNode.textContent : el.textContent,
      {heading:!!el.closest('h1,h2,h3,h4,h5,h6,title')});
  }
  for (const el of document.querySelectorAll('canvas, iframe, object, embed')) if (visible(el)) limitations.push({kind:'unreadable-content', location:location(el)});
  return {version:1, tables, texts, units, limitations};
}

const CODE = /^[A-Z][A-Z0-9]*$/;
const TBD = /^\[?TBD\]?$/i;
const UNIT = /^(px|mm|cm|in|pt|rem|em|vh|vw|vmin|vmax|%|deg|s|ms)(?![a-z])/i;
const absolute = {px:1, mm:96/25.4, cm:96/2.54, in:96, pt:96/72, s:1, ms:.001};
const family = u => ['mm','cm','in','pt'].includes(u) ? 'px' : u === 'ms' ? 's' : u;
const issue = (reason, status='UNKNOWN') => { throw Object.assign(new Error(reason), {status}); };
const quantity = (lo, hi=lo, unit='') => {
  if (!Number.isFinite(lo) || !Number.isFinite(hi)) return issue('non-finite arithmetic','FAIL');
  if (lo > hi) return issue('reversed range','FAIL');
  const scale = absolute[unit] || 1;
  return {lo:lo*scale, hi:hi*scale, unit:family(unit)};
};
const withDefault = (q, unit) => !q.unit && unit ? quantity(q.lo,q.hi,unit) : q;
const compatible = (a,b) => {
  if (!a.unit) a = {...a,unit:b.unit};
  if (!b.unit) b = {...b,unit:a.unit};
  if (a.unit !== b.unit) {
    const dimension = u => u==='s' ? 'time' : u==='deg' ? 'angle' : u==='%' ? 'ratio' : 'length';
    if (dimension(a.unit)!==dimension(b.unit) && ![a.unit,b.unit].includes('%')) issue(`incompatible units: ${a.unit} and ${b.unit}`,'FAIL');
    issue(`unresolved units: ${a.unit} and ${b.unit}`);
  }
  return [a,b];
};
function arithmetic(op,a,b) {
  if (a.lo!==a.hi && b.lo!==b.hi) issue('range operands may be correlated');
  if (op === '+' || op === '-') {
    [a,b] = compatible(a,b);
    return quantity(op==='+' ? a.lo+b.lo : a.lo-b.hi, op==='+' ? a.hi+b.hi : a.hi-b.lo,a.unit);
  }
  if (a.unit==='%') a = quantity(a.lo/100,a.hi/100);
  if (b.unit==='%') b = quantity(b.lo/100,b.hi/100);
  let unit;
  if (op === '*') {
    if (a.unit && b.unit) issue('multiplication of two dimensional values');
    unit = a.unit || b.unit;
  } else {
    if (b.lo <= 0 && b.hi >= 0) issue('division by zero','FAIL');
    if (b.unit && a.unit !== b.unit) issue('division by an incompatible dimension');
    unit = b.unit ? '' : a.unit;
  }
  const vs = [a.lo,a.hi].flatMap(x => [b.lo,b.hi].map(y => op==='*' ? x*y : x/y));
  return quantity(Math.min(...vs),Math.max(...vs),unit);
}

// Recursive descent over a closed token vocabulary. No code execution or property access.
function expression(input, resolve) {
  const s = input.replace(/−/g,'-').replace(/[·×]/g,'*').replace(/÷/g,'/').trim();
  if (s.length > 2048) issue('expression too long');
  let at = 0, tokens = 0, depth = 0;
  const space = () => { while (/\s/.test(s[at] || '') && at < s.length) at++; };
  const primary = () => {
    if (++tokens > 256 || ++depth > 32) issue('expression complexity limit');
    space(); let q;
    if (s[at]==='+' || s[at]==='-') {
      const sign = s[at++]; q = primary(); if (sign==='-') q = quantity(-q.hi,-q.lo,q.unit);
    } else if (s[at]==='(') {
      at++; q = sum(); space(); if (s[at++]!==')') issue('unclosed parentheses');
    } else {
      const number = s.slice(at).match(/^(?:\d+(?:\.\d*)?|\.\d+)/);
      if (number) {
        at += number[0].length; space(); const u = s.slice(at).match(UNIT);
        if (u) at += u[0].length;
        q = quantity(Number(number[0]),Number(number[0]),u ? u[0].toLowerCase() : '');
      } else {
        const id = s.slice(at).match(/^[A-Za-z][A-Za-z0-9]*/);
        if (!id) issue('unsupported expression token');
        at += id[0].length; space();
        if (s[at]==='(') issue('unsupported function syntax');
        q = resolve(id[0]);
      }
    }
    depth--; return q;
  };
  const product = () => {
    let q = primary(); space();
    while (s[at]==='*' || s[at]==='/') { const op = s[at++]; q = arithmetic(op,q,primary()); space(); }
    return q;
  };
  const sum = () => {
    let q = product(); space();
    while (s[at]==='+' || s[at]==='-') { const op=s[at++]; q=arithmetic(op,q,product()); space(); }
    return q;
  };
  const q = sum(); space(); if (at !== s.length) issue('unsupported expression suffix'); return q;
}
function parseValue(raw, resolve, defaultUnit) {
  const text = String(raw).trim().replace(/\s*(?:[·,]\s*)?fixed\s*$/i,'');
  if (TBD.test(text)) issue('TBD operand');
  const range = text.match(/^([+−-]?(?:\d+(?:\.\d+)?|\.\d+))\s*([a-z%]*)\s*(?:[–—]|\.\.)\s*([+−-]?(?:\d+(?:\.\d+)?|\.\d+))\s*([a-z%]*)$/i);
  if (range) {
    const [,a,ua,b,ub] = range;
    if ([ua,ub].some(u => u && !new RegExp(`^(?:${UNIT.source.slice(1)})$`,'i').test(u))) issue('unsupported range unit');
    const qa=quantity(Number(a.replace('−','-')),undefined,(ua||ub||defaultUnit||'').toLowerCase());
    const qb=quantity(Number(b.replace('−','-')),undefined,(ub||ua||defaultUnit||'').toLowerCase());
    const [x,y]=compatible(qa,qb); return quantity(x.lo,y.hi,x.unit);
  }
  return withDefault(expression(text,resolve),defaultUnit);
}
const equal = (a,b) => {
  [a,b]=compatible(a,b);
  const close = (x,y) => Math.abs(x-y) <= 1e-7*Math.max(1,Math.abs(x),Math.abs(y));
  return close(a.lo,b.lo) && close(a.hi,b.hi);
};
const references = (text, known) => {
  const content = String(text)
    .replace(/\b[A-Z]{2,}-[A-Z][A-Z0-9-]*-\d{2,}\b/g,'')
    .replace(/\b(?:REV(?:ISION)?\.?|DETAIL|SECTION)\s+[A-Z]\b/g,'');
  return [...content.matchAll(/(?<![A-Za-z0-9_])[A-Z][A-Z0-9]*(?![A-Za-z0-9_])/g)]
    .map(m=>m[0]).filter(c => known.has(c) || ['B','W','Y','BL','SP','MH'].includes(c) || (/^[A-Z]{1,4}\d+$/.test(c) && !/^L\d+$/.test(c)));
};

export function expandDefinitionRows(rows) {
  return rows.flatMap(row => {
    if (!/^[A-Z][A-Z0-9]*(?:\s*\/\s*[A-Z][A-Z0-9]*)+$/.test(row.code)) return [row];
    const codes=row.code.split(/\s*\/\s*/),names=row.variable.split(/\s*\/\s*/),values=row.value.split(/\s*\/\s*/);
    return codes.length===names.length && codes.length===values.length
      ? codes.map((code,i)=>({...row,code,variable:names[i],value:values[i]})) : [row];
  });
}

export function checkDrawingFacts(observation, sourceFacts = {}) {
  const findings = [];
  const add = (status,kind,evidence={}) => findings.push({kind,status,...evidence});
  if (!observation || observation.version!==1 || !Array.isArray(observation.tables) || !Array.isArray(observation.texts)) {
    return {status:'UNKNOWN',findings:[{kind:'invalid-observation',status:'UNKNOWN'}]};
  }
  for (const limitation of observation.limitations || []) add('UNKNOWN','collection-limitation',limitation);
  if (!observation.tables.length) add('UNKNOWN','missing-variable-table');
  const partialTables = !observation.tables.length || (observation.limitations || []).some(l=>/table|cell|row/.test(l.kind)) ||
    observation.tables.some(t=>expandDefinitionRows(t.rows || []).some(r=>!CODE.test(r.code)));
  const partialText = (observation.limitations || []).length > 0;
  const definitions = new Map(), uses = new Map();
  for (const table of observation.tables) {
    if (!table.rows?.length) add('UNKNOWN','empty-variable-table',{location:table.location});
    for (const row of expandDefinitionRows(table.rows || [])) {
      if (!CODE.test(row.code)) { add('UNKNOWN','ambiguous-code',{...row}); continue; }
      if (!row.variable || !row.value) add('FAIL','incomplete-definition',{...row});
      if (definitions.has(row.code)) add('FAIL','duplicate-definition',{code:row.code,first:definitions.get(row.code),duplicate:row});
      else definitions.set(row.code,row);
    }
  }
  const known = new Set(definitions.keys());
  const use = (code,evidence) => {
    if (!uses.has(code)) uses.set(code,[]);
    uses.get(code).push(evidence);
    if (!known.has(code)) {
      const axisAmbiguity=code==='Y' && /\bX\s+[−+\d]/.test(evidence.text || '') && /\bY\s+[−+\d]/.test(evidence.text || '');
      add(partialTables || axisAmbiguity ? 'UNKNOWN':'FAIL',axisAmbiguity ? 'ambiguous-coordinate-symbol':'undefined-code',{...evidence,code});
    }
  };
  for (const t of observation.texts) for (const code of references(t.text,known,t.heading)) use(code,t);
  for (const row of definitions.values()) for (const code of references(row.value,known)) use(code,{region:'table',location:row.location,definition:row.code,text:row.value});
  for (const [code,row] of definitions) if (!uses.has(code)) add(partialText ? 'UNKNOWN':'FAIL','unused-definition',{code,location:row.location});
  const pageUnits = [...new Set((observation.units || []).map(u => {
    const s=u.text.toLowerCase();
    if (/\b(?:css\s*)?px\b/.test(s)) return 'px';
    return ['mm','cm','in','pt'].find(unit=>new RegExp(`\\b${unit}\\b`).test(s)) || null;
  }))];
  if (pageUnits.includes(null) || pageUnits.length>1) add('UNKNOWN','ambiguous-default-unit',{units:observation.units});
  const defaultUnit = pageUnits.length===1 ? pageUnits[0] || '' : '';
  let source = sourceFacts?.values || {};
  if (typeof source !== 'object' || Array.isArray(source)) { add('UNKNOWN','invalid-source-facts'); source = {}; }
  const sourceUnit = sourceFacts?.defaultUnit || '';
  const validSourceUnit = !sourceUnit || new RegExp(`^(?:${UNIT.source.slice(1)})$`,'i').test(sourceUnit);
  if (!validSourceUnit) add('UNKNOWN','invalid-source-unit',{unit:sourceUnit});
  const cache = new Map(), resolving = new Set();
  const resolve = code => {
    if (cache.has(code)) return cache.get(code);
    const row = definitions.get(code);
    if (!row) issue(`unresolved symbol: ${code}`, !partialTables && references(code,known).length ? 'FAIL':'UNKNOWN');
    if (resolving.has(code)) issue(`cyclic formula: ${code}`);
    if (resolving.size > 128) issue('formula dependency depth limit');
    resolving.add(code);
    try {
      const parts=row.value.split('=');
      if(parts.length>1 && parts[0].trim()===code) parts.shift();
      const qs=parts.map(part=>parseValue(part,resolve,defaultUnit));
      if(qs.some(q=>!equal(qs[0],q))) issue(`formula equality differs: ${code}`,'FAIL');
      const q=qs.at(-1);
      cache.set(code,q); return q;
    } finally { resolving.delete(code); }
  };
  const attempt = (raw,unit,evidence,resolver=resolve) => {
    try { return parseValue(raw,resolver,unit); }
    catch (err) { add(err.status || 'UNKNOWN','unresolved-value',{...evidence,raw,reason:err.message}); return null; }
  };
  const compare = (a,b,kind,evidence) => {
    if (!a || !b) return;
    try { if (!equal(a,b)) add('FAIL',kind,{...evidence,actual:a,expected:b}); }
    catch (err) { add(err.status || 'UNKNOWN','unresolved-comparison',{...evidence,reason:err.message,actual:a,expected:b}); }
  };
  for (const [code,row] of definitions) {
    const evidence = {code,location:row.location,value:row.value};
    const hasSource = Object.hasOwn(source,code) && source[code] != null && !TBD.test(String(source[code]));
    if (TBD.test(row.value)) {
      if (hasSource) add('FAIL','known-source-marked-tbd',{...evidence,source:source[code]});
      continue;
    }
    const parts=row.value.split('='), qs=parts.map(p=>attempt(p,defaultUnit,evidence));
    const firstKnown = qs.find(Boolean);
    qs.forEach(q=>compare(firstKnown,q,'formula-mismatch',evidence));
    if (!defaultUnit && qs.some(q=>q && !q.unit)) add('UNKNOWN','missing-default-unit',evidence);
    if (hasSource) {
      const expected = validSourceUnit ? attempt(source[code],sourceUnit.toLowerCase(),{code,region:'independent-source'},id=>issue(`source fact is not literal: ${id}`)) : null;
      if (expected && !expected.unit && !sourceUnit) add('UNKNOWN','missing-source-unit',{code,source:source[code]});
      qs.forEach(q=>compare(q,expected,'source-mismatch',{...evidence,source:source[code]}));
    }
  }
  for (const [code,value] of Object.entries(source)) if (!known.has(code) && value != null) add(partialTables ? 'UNKNOWN':'FAIL','missing-source-definition',{code,source:value});
  for (const t of observation.texts) for (const clause of t.text.split(/[;|]/)) {
    const claim=clause.trim().match(/\b([A-Z][A-Z0-9]*)\s*(?:[=:]\s*(.+)|\s+(\[TBD\]|[+−-]?(?:\d|\.).*))$/);
    if (!claim || !known.has(claim[1])) continue;
    const code=claim[1], value=claim[2] || claim[3], row=definitions.get(code);
    if (TBD.test(value)) {
      if (!TBD.test(row.value)) add('FAIL','claim-mismatch',{code,...t,tableValue:row.value});
    } else if (TBD.test(row.value)) add('FAIL','claim-invents-value',{code,...t,tableValue:row.value});
    else {
      const qs=value.split('=').map(p=>attempt(p,defaultUnit,{code,...t}));
      const expected=attempt(code,defaultUnit,{code,location:row.location});
      qs.forEach(q=>compare(q,expected,'claim-mismatch',{code,...t,tableValue:row.value}));
    }
  }
  return {status: findings.some(f=>f.status==='FAIL') ? 'FAIL' : findings.some(f=>f.status==='UNKNOWN') ? 'UNKNOWN' : 'PASS',
    findings, counts:{tables:observation.tables.length,definitions:definitions.size,usedCodes:uses.size},
    sourceCoverage:{known:Object.keys(source).filter(c=>source[c]!=null).length,definitions:definitions.size}};
}
