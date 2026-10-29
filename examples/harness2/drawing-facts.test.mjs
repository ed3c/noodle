import test, {before, after} from 'node:test';
import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
import {collectDrawingFacts, checkDrawingFacts} from './drawing-facts.mjs';

// Use the repository's existing browser dependency and actual DOM geometry.
const require = createRequire(new URL('../../e2e/ui/package.json', import.meta.url));
const {chromium} = require('@playwright/test');
let browser;
before(async () => { browser = await chromium.launch({headless:true}); });
after(async () => { await browser?.close(); });
const rowsHTML = rows => `<table><tr><th>Code</th><th>Variable</th><th>Value</th></tr>${rows.map(([c,n,v])=>`<tr><td>${c}</td><td>${n}</td><td>${v}</td></tr>`).join('')}</table>`;
const sheet = (rows, labels='', extra='') => `<header><div>Units</div><div>CSS px @ 1×</div></header>${rowsHTML(rows)}<svg>${labels.split('|').map(s=>`<text>${s}</text>`).join('')}</svg>${extra}`;
async function collect(html) {
  const page = await browser.newPage();
  try {
    await page.setContent(html);
    // Playwright serializes the function, without module imports or closures.
    return await page.evaluate(collectDrawingFacts);
  } finally { await page.close(); }
}
const check = async (html, source) => checkDrawingFacts(await collect(html), source);
const kinds = result => result.findings.map(f=>f.kind);
const row = (c,v) => [c,`variable-${c.toLowerCase()}`,v];
function svgTable(rows, x=20, y=20, attributes='') {
  const ws=[40,88,96], rh=17;
  return `<g ${attributes}>${[['Code','Variable','Value'],...rows].map((r,i)=>{
    let cx=x;
    return r.map((v,j)=>{
      const value=`<rect x="${cx}" y="${y+i*rh}" width="${ws[j]}" height="${rh}"/><text x="${cx+(j ? ws[j]/2:6)}" y="${y+i*rh+12}" text-anchor="${j?'middle':'start'}">${v}</text>`;
      cx+=ws[j]; return value;
    }).join('');
  }).join('')}</g>`;
}

test('HTML tables, summary repeats, footer claims and source facts agree', async () => {
  const html=sheet([row('S1','14'),row('H3','36'),row('H1','2·S1 + H3 = 64')],'S1|H3|H1',
    '<header><div class="figs"><div><div class="lbl">H1 · header-min</div><div class="v">2·S1 + H3</div></div></div></header><footer>H1 = 64</footer>');
  const result=(await check(html,{values:{S1:'14 px',H1:'64 px'},defaultUnit:'px'}));
  assert.equal(result.status,'PASS',JSON.stringify(result));
  assert.equal(result.counts.definitions,3);
  assert.equal((await collect(html)).texts.filter(t=>t.region==='footer').length,1);
});

test('SVG tables use headers and cell geometry, preserving rotated uses', async () => {
  const html=`<header><div>Units</div><div>px</div></header><svg>${svgTable([row('VW1','420'),row('X2','12')])}<text x="500" y="100" transform="rotate(-90 500 100)">VW1</text><text x="550" y="100">X2</text></svg>`;
  const observation=(await collect(html));
  assert.deepEqual(observation.tables[0].rows.map(r=>r.code),['VW1','X2']);
  assert.equal(observation.texts.filter(t=>t.region==='drawing').length,2);
  assert.equal(checkDrawingFacts(observation).status,'PASS');
});

test('old Codex omission pattern VW1, VW2, X1, X2 fails with locations', async () => {
  const result=(await check(sheet([row('W','420')],'W|VW1|VW2|X1|X2')));
  assert.equal(result.status,'FAIL');
  assert.deepEqual(result.findings.filter(f=>f.kind==='undefined-code').map(f=>f.code),['VW1','VW2','X1','X2']);
  assert.ok(result.findings.filter(f=>f.kind==='undefined-code').every(f=>f.region==='drawing' && f.location));
});

test('outlined SVG path grids reconcile without worker table metadata', async () => {
  const html=`<p>Units</p><p>px</p><svg width="600" height="200"><g transform="translate(30 20)">
    <rect width="300" height="90" fill="none" stroke="black"/>
    <path d="M60 0V90M160 0V90M0 30H300M0 60H300" fill="none" stroke="black"/>
    <text x="8" y="20">Code</text><text x="70" y="20">Variable</text><text x="170" y="20">Value</text>
    <text x="8" y="50">W1</text><text x="70" y="50">width</text><text x="170" y="50">480</text>
    <text x="8" y="80">MH / OF</text><text x="70" y="80">height / overflow</text><text x="170" y="80">[TBD] / [TBD]</text></g>
    <text x="400" y="30">W1</text><text x="400" y="60">MH</text><text x="400" y="90">OF</text><text x="400" y="120">VW1</text></svg>`;
  const observation=await collect(html);
  assert.equal(observation.tables.length,1);
  assert.equal(observation.tables[0].rows.length,2);
  const result=checkDrawingFacts(observation);
  assert.equal(result.status,'FAIL',JSON.stringify(result));
  assert.deepEqual(result.findings.filter(f=>f.kind==='undefined-code').map(f=>f.code),['VW1']);
  assert.equal(result.counts.definitions,3);
});

test('title, drawing ID, slot words, layer numbers and viewport units are not codes', async () => {
  const result=(await check(sheet([row('W','420')],'W|100VW|100VH|L2 · SHADOW|TITLE SLOT · FLEX|UI-CARD-001',
    '<h1>COMPONENT DRAWING</h1><footer>UI-CARD-001 · REV B</footer>')));
  // B in an explicit revision is not a border-dimension use.
  assert.equal(result.status,'PASS',JSON.stringify(result));
});

test('title tags and title classes cannot conceal dimension uses', async () => {
  const result=await check(sheet([row('W','420')],'W|VW1', '<h1>W = 421</h1>'));
  assert.equal(result.status,'FAIL');
  assert.ok(result.findings.some(f=>f.kind==='undefined-code' && f.code==='VW1'));
  assert.ok(result.findings.some(f=>f.kind==='claim-mismatch' && f.code==='W'));
  const svg=await check(`<svg>${svgTable([row('W','420')])}<text x="500" y="100" class="vt">VW2</text><text x="500" y="150">W</text></svg><p>Units</p><p>px</p>`);
  assert.ok(svg.findings.some(f=>f.kind==='undefined-code' && f.code==='VW2'));
});

test('layout whitespace and direct body text do not concatenate tables into fabricated codes', async () => {
  const observation=await collect(`\n${sheet([row('W','420')],'W')}\nReference\n<div>Footer</div>\n`);
  assert.ok(observation.texts.every(t=>!t.text.includes('CodeVariableValue')));
  assert.equal(checkDrawingFacts(observation).status,'PASS');
});

test('an unresolved multi-reference formula is UNKNOWN, not a fabricated numeric disagreement', async () => {
  const result=await check(sheet([row('VW','1280 / 1800 · REF'),row('W','384'),row('X1','(VW-W)/2 = 448 / 708')],
    'VW|W|X1 = (VW-W)/2 = 708'));
  assert.equal(result.status,'UNKNOWN',JSON.stringify(result));
  assert.ok(!result.findings.some(f=>f.kind==='claim-mismatch'));
});

test('an undeclared coordinate Y is ambiguous only with explicit numeric X/Y coordinates', async () => {
  const result=await check(sheet([row('W','420')],'W|X 10 Y 32'));
  assert.equal(result.status,'UNKNOWN',JSON.stringify(result));
  assert.ok(result.findings.some(f=>f.kind==='ambiguous-coordinate-symbol'));
  assert.equal((await check(sheet([row('W','420')],'W|Y = 32'))).status,'FAIL');
});

test('definitions are checked in the other direction too', async () => {
  const result=(await check(sheet([row('W','420'),row('S9','12')],'W')));
  assert.equal(result.status,'FAIL');
  assert.ok(result.findings.some(f=>f.kind==='unused-definition' && f.code==='S9'));
});

test('summary mentions are uses but separate actual tables are duplicate definitions', async () => {
  const result=(await check(sheet([row('S1','14')],'S1',rowsHTML([row('S1','14')]))));
  assert.equal(result.status,'FAIL');
  assert.ok(result.findings.some(f=>f.kind==='duplicate-definition' && f.first.location && f.duplicate.location));
});

test('formula arithmetic disagreement fails', async () => {
  const result=(await check(sheet([row('S1','14'),row('H3','36'),row('H1','2·S1+H3 = 63')],'S1|H3|H1')));
  assert.equal(result.status,'FAIL'); assert.ok(kinds(result).includes('formula-mismatch'));
});

test('operators, parentheses, precedence, signed values and unit cancellation', async () => {
  const result=(await check(sheet([row('S1','14 px'),row('H3','36 px'),row('H1','(2×S1+H3)÷2 = 32 px'),row('Y','−16 px'),row('SP','H3 / S1 * 7 = 18')],'S1|H3|H1|Y|SP')));
  assert.equal(result.status,'PASS',JSON.stringify(result));
});

test('ranges and absolute unit conversion compare endpoints', async () => {
  assert.equal((await check(sheet([row('W','420–680 px')],'W'),{values:{W:'420 px–680 px'}})).status,'PASS');
  assert.equal((await check(sheet([row('W','1 in')],'W'),{values:{W:'25.4 mm'}})).status,'PASS');
  const result=(await check(sheet([row('W','420–680')],'W'),{values:{W:'420–700 px'}}));
  assert.equal(result.status,'FAIL'); assert.ok(kinds(result).includes('source-mismatch'));
});

test('range with mismatched endpoint units is not silently coerced', async () => {
  assert.equal((await check(sheet([row('W','1 px–3 vh')],'W'))).status,'UNKNOWN');
});

test('vh and percentage units support literal comparison and scaling', async () => {
  assert.equal((await check(sheet([row('MH','100vh * 0.5 = 50vh')],'MH'),{values:{MH:'50 vh'}})).status,'PASS');
  assert.equal((await check(sheet([row('W','50% * 200px = 100px')],'W'))).status,'PASS');
  assert.equal((await check(sheet([row('W','50%')],'W'),{values:{W:'60%'}})).status,'FAIL');
});

test('mixed viewport and absolute values need independent context', async () => {
  const result=(await check(sheet([row('V1','14'),row('MH','100vh − 2·V1')],'MH|V1')));
  assert.equal(result.status,'UNKNOWN'); assert.ok(kinds(result).includes('unresolved-value'));
});

test('independent source contradiction fails, also for a derived claim', async () => {
  const result=(await check(sheet([row('S1','14'),row('H3','36'),row('H1','2*S1+H3=64')],'S1|H3|H1'),{values:{H1:'65 px'}}));
  assert.equal(result.status,'FAIL'); assert.ok(kinds(result).includes('source-mismatch'));
});

test('legitimate TBD is PASS with absent or null source values', async () => {
  const html=sheet([row('V1','[TBD]')],'V1 [TBD]');
  assert.equal((await check(html)).status,'PASS');
  assert.equal((await check(html,{values:{V1:null}})).status,'PASS');
  assert.equal((await check(html,{values:{V1:'14 px'}})).status,'FAIL');
});

test('an explicitly required source-unknown dimension cannot disappear from the table', async () => {
  const html=sheet([row('W1','480 px')],'W1');
  const result=await check(html,{values:{W1:'480 px',V1:null}});
  assert.equal(result.status,'FAIL');
  assert.ok(result.findings.some(f=>f.kind==='missing-source-definition' && f.code==='V1'));
});

test('TBD dependency makes a formula UNKNOWN, not false PASS', async () => {
  const result=(await check(sheet([row('V1','[TBD]'),row('MH','100vh−2·V1')],'MH|V1')));
  assert.equal(result.status,'UNKNOWN');
});

test('unsupported grammar and contextual symbols stay UNKNOWN', async () => {
  for (const value of ['h ÷ 2','min(40px, 50px)','MIN(40px, 50px)','AUTO','auto','calc(100vh - 2px)','12 bananas']) {
    assert.equal((await check(sheet([row('R4',value)],'R4'))).status,'UNKNOWN',value);
  }
});

test('cycles cannot recurse forever', async () => {
  const result=(await check(sheet([row('R1','R2 + 1'),row('R2','R1 - 1')],'R1|R2')));
  assert.equal(result.status,'UNKNOWN'); assert.ok(result.findings.some(f=>/cyclic/.test(f.reason)));
});

test('undefined table formula operands fail even without external uses', async () => {
  const result=(await check(sheet([row('H1','2*S1 + 36 = 64')],'H1')));
  assert.equal(result.status,'FAIL');
  assert.ok(result.findings.some(f=>f.kind==='undefined-code' && f.code==='S1' && f.region==='table'));
});

test('FAIL wins over UNKNOWN and a known RHS contradiction survives an unknown LHS', async () => {
  const result=(await check(sheet([row('R4','h/2 = 64')],'R4|VW2'),{values:{R4:'65 px'}}));
  assert.equal(result.status,'FAIL');
  assert.ok(result.findings.some(f=>f.status==='UNKNOWN'));
  assert.ok(kinds(result).includes('source-mismatch'));
});

test('zero division, reversed ranges and non-finite arithmetic fail', async () => {
  for (const value of ['14 / 0','680–420','1 / (2-2)']) assert.equal((await check(sheet([row('W',value)],'W'))).status,'FAIL',value);
});

test('malicious expressions are never executed', async () => {
  const result=(await check(sheet([row('W','globalThis.pwned=1')],'W')));
  assert.notEqual(result.status,'PASS'); assert.equal(globalThis.pwned,undefined);
});

test('page metadata cannot supply definitions or source truth', async () => {
  const html='<script type="application/json">{"tables":[{"code":"W","value":"420"}]}</script><meta name="sourceFacts" content="W=420"><svg><text>W</text></svg>';
  const observation=(await collect(html));
  assert.equal(observation.tables.length,0);
  assert.ok(kinds(checkDrawingFacts(observation)).includes('missing-variable-table'));
});

test('missing, empty, malformed or geometrically unrecognized tables never PASS', async () => {
  for (const html of ['<h1>DRAWING</h1>',rowsHTML([]),'<svg><text>Code</text><text>Variable</text><text>Value</text></svg>']) {
    assert.equal((await check(html)).status,'UNKNOWN',html);
  }
  assert.equal(checkDrawingFacts({}).status,'UNKNOWN');
});

test('footer/header values are claims, with known contradictions detected', async () => {
  for (const place of ['header','footer']) {
    const result=(await check(sheet([row('W','420')],'W',`<${place}>W = 421 px</${place}>`)));
    assert.equal(result.status,'FAIL'); assert.ok(kinds(result).includes('claim-mismatch'));
  }
});

test('incomplete rows fail rather than losing missing values', async () => {
  const result=(await check(sheet([['W','','']],'W')));
  assert.equal(result.status,'FAIL'); assert.ok(kinds(result).includes('incomplete-definition'));
});

test('unreadable content produces an explicit limit and hidden metadata is ignored', async () => {
  const result=(await check(sheet([row('W','420')],'W','<canvas></canvas><div style="display:none">VW1 = 300</div>')));
  assert.equal(result.status,'UNKNOWN');
  assert.ok(result.findings.some(f=>f.kind==='unreadable-content'));
  assert.ok(!kinds(result).includes('undefined-code'));
});

test('unruled capital strings cannot masquerade as an SVG variable table', async () => {
  const observation=(await collect('<svg><text x="0" y="10">Code</text><text x="50" y="10">Variable</text><text x="100" y="10">Value</text><text x="0" y="30">VW1</text><text x="100" y="30">420</text></svg>'));
  assert.equal(observation.tables.length,0);
  assert.equal(checkDrawingFacts(observation).status,'UNKNOWN');
});

test('separate SVG tables remain separate and duplicate codes fail', async () => {
  const html=`<svg>${svgTable([row('W','420')])}${svgTable([row('W','420')],300)}<text x="600" y="50">W</text></svg>`;
  const observation=(await collect(html));
  assert.equal(observation.tables.length,2);
  assert.ok(kinds(checkDrawingFacts(observation)).includes('duplicate-definition'));
});


test('ASCII subtraction does not hide references or create unused definitions', async () => {
  const result=await check(sheet([row('R1','16'),row('B','1'),row('S6','4'),row('R2','R1-B-S6=11')],'R2'));
  assert.equal(result.status,'PASS',JSON.stringify(result));
});

test('SVG table coordinates include translated and scaled ancestors', async () => {
  const html=`<svg>${svgTable([row('W','420 px')],20,20,'transform="translate(100 50) scale(1.5)"')}<text x="600" y="100">W</text></svg>`;
  const result=await check(html);
  assert.equal(result.status,'PASS',JSON.stringify(result));
});

test('inline spans cannot hide a contradictory footer claim', async () => {
  const result=await check(sheet([row('W','420')],'W','<footer><span>W</span><span> = </span><strong>421 px</strong></footer>'));
  assert.equal(result.status,'FAIL'); assert.ok(kinds(result).includes('claim-mismatch'));
});

test('claims embedded in a note are still reconciled', async () => {
  const result=await check(sheet([row('W','420')],'W','<p>Overall width W = 421 px</p>'));
  assert.equal(result.status,'FAIL'); assert.ok(kinds(result).includes('claim-mismatch'));
});

test('unknown table structure does not assert that a code is absent', async () => {
  const result=await check(sheet([row('W','420')],'W|X1','<table><tr><td>X1</td><td>14</td></tr></table>'));
  assert.equal(result.status,'UNKNOWN');
  assert.ok(result.findings.some(f=>f.kind==='undefined-code' && f.code==='X1' && f.status==='UNKNOWN'));
});

test('known contradictions still FAIL with incomplete table collection', async () => {
  const result=await check(sheet([row('W','420')],'W|X1','<table><tr><td>unknown structure</td></tr></table>'),{values:{W:'421 px'}});
  assert.equal(result.status,'FAIL'); assert.ok(kinds(result).includes('source-mismatch'));
});

test('unsupported LHS cannot hide contradictory numeric equalities', async () => {
  const result=await check(sheet([row('R4','h/2 = 64 = 65')],'R4'));
  assert.equal(result.status,'FAIL'); assert.ok(kinds(result).includes('formula-mismatch'));
});

test('missing unit declaration is UNKNOWN; incompatible dimensions FAIL', async () => {
  assert.equal((await check(rowsHTML([row('W','420')])+'<svg><text>W</text></svg>')).status,'UNKNOWN');
  const result=await check(sheet([row('W','420 px')],'W'),{values:{W:'420 s'}});
  assert.equal(result.status,'FAIL');
});

test('missing independent source units and malformed source maps are UNKNOWN', async () => {
  const html=sheet([row('W','420')],'W');
  assert.equal((await check(html,{values:{W:'420'}})).status,'UNKNOWN');
  assert.equal((await check(html,{values:'worker metadata'})).status,'UNKNOWN');
  assert.equal((await check(html,{values:{W:'420'},defaultUnit:'bananas'})).status,'UNKNOWN');
});


test('separate claims in one footer reconcile against their own codes', async () => {
  const rows=[row('W','420'),row('S1','14')];
  assert.equal((await check(sheet(rows,'W|S1','<footer>W = 420 px | S1 = 14 px</footer>'))).status,'PASS');
  const result=await check(sheet(rows,'W|S1','<footer>W = 420 px; S1 = 15 px</footer>'));
  assert.equal(result.status,'FAIL');
  assert.ok(result.findings.some(f=>f.kind==='claim-mismatch' && f.code==='S1'));
});

test('simple colon/space value labels are checked too', async () => {
  for (const label of ['W: 421 px','W 421 px']) {
    const result=await check(sheet([row('W','420')],label));
    assert.equal(result.status,'FAIL'); assert.ok(kinds(result).includes('claim-mismatch'));
  }
});

test('correlated range arithmetic is UNKNOWN rather than a false mismatch', async () => {
  const result=await check(sheet([row('W','420–680'),row('X1','W-W=0')],'W|X1'));
  assert.equal(result.status,'UNKNOWN',JSON.stringify(result));
});

test('unknown table coverage applies to unresolved formula operands too', async () => {
  const result=await check(sheet([row('H1','2*S1+36')],'H1','<table><tr><td>S1</td><td>14</td></tr></table>'));
  assert.equal(result.status,'UNKNOWN',JSON.stringify(result));
});

test('SVG-only title-block units supply the bare-value unit', async () => {
  const html=`<svg>${svgTable([row('W','420')])}<text x="400" y="100">W</text><text x="400" y="200">UNITS</text><text x="400" y="220">CSS PX</text></svg>`;
  assert.equal((await check(html,{values:{W:'420 px'}})).status,'PASS');
});
