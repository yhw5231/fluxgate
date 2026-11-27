'use strict';

// Run with: node internal/console/app_behavior_test.cjs
// The suite uses only Node built-ins. Go skips it when Node is unavailable.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const original = fs.readFileSync(path.join(__dirname, 'static/app.js'), 'utf8');

function loadConsole() {
  const document = {
    readyState: 'loading', activeElement: null,
    addEventListener() {},
    createElement(tag) { return new Node(tag); },
    createTextNode(text) { const node = new Node('#text'); node.textContent = text; return node; },
    querySelectorAll(selector) { return selector === '[data-probe-site]' ? siteButtons : []; },
    querySelector() { return null; }
  };
  const siteButtons = [];
  class Node {
    constructor(tag) {
      this.tagName = tag;
      this.children = [];
      this.attributes = {};
      this.listeners = {};
      this.disabled = false;
      this.hidden = false;
      this.className = '';
      this.value = '';
      this.ownText = '';
    }
    set textContent(value) { this.ownText = String(value); this.children = []; }
    get textContent() { return this.ownText + this.children.map(child => child.textContent).join(''); }
    appendChild(child) { this.children.push(child); return child; }
    setAttribute(name, value) { this.attributes[name] = String(value); }
    getAttribute(name) { return this.attributes[name]; }
    addEventListener(name, handler) { this.listeners[name] = handler; }
    focus() { document.activeElement = this; }
    contains(node) { return this === node || this.children.some(child => child.contains(node)); }
    querySelectorAll(selector) {
      const found = [];
      function visit(node) {
        node.children.forEach(child => {
          if ((selector === '.btn-label' && child.className.split(/\s+/).includes('btn-label')) ||
            (selector === '.field-help' && child.className.split(/\s+/).includes('field-help')) ||
            (selector.startsWith('button:not(') && ((child.tagName === 'button' && !child.disabled) || child.getAttribute('tabindex') === '0'))) found.push(child);
          visit(child);
        });
      }
      visit(this);
      return found;
    }
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  }
  const requests = [];
  const context = {
    document, console,
    window: { location: { pathname: '/gateway/console/' } },
    fetch(url, options) {
      return new Promise((resolve, reject) => requests.push({
        url, options, reject,
        respond(status, body) { resolve({ status, ok: status >= 200 && status < 300, json: () => Promise.resolve(body) }); }
      }));
    }
  };
  const marker = /  if \(document\.readyState === 'loading'\)/;
  assert.match(original, marker, 'bootstrap marker must be unique and present');
  const instrumented = original.replace(marker, `globalThis.consoleUnderTest={state,els,FIELDS,RESOURCES,breakerLineCell,breakerResetTarget,
    runProbeSweep,probeUpstreamNow,probeChannelNow,actionCell,buildField,createKeysField,createModelPicker,
    openUpstreamHelp,closeUpstreamHelp,upstreamHelpKeydown,renderUpstreamHelp,renderBreakers,
    stubEffects(effects){showNotice=effects.notice;showBanner=effects.banner;showAuth=effects.auth;refresh=effects.refresh;}};
    if (document.readyState === 'loading')`);
  vm.runInNewContext(instrumented, context, { filename: 'app.js' });
  const ui = context.consoleUnderTest;
  const effects = { notices: [], banners: [], auth: [], refreshes: 0 };
  ui.stubEffects({
    notice: message => effects.notices.push(message),
    banner: (title, detail) => effects.banners.push({title, detail}),
    auth: message => effects.auth.push(message),
    refresh: () => { effects.refreshes++; }
  });
  function button(label) {
    const node = new Node('button');
    const text = new Node('span'); text.className = 'btn-label'; text.textContent = label;
    node.appendChild(text); return node;
  }
  ui.els.probesRunAll = button('探测全部线路');
  ui.els.upstreamsProbeAll = button('检测全部');
  ui.els.upstreamsHelp = button('帮助');
  ui.els.upstreamsHelpClose = button('关闭');
  ui.els.upstreamsHelpFields = new Node('div');
  ui.els.upstreamsHelpContent = new Node('div');
  ui.els.upstreamsHelpContent.setAttribute('tabindex', '0');
  ui.els.upstreamsHelpContent.appendChild(ui.els.upstreamsHelpFields);
  ui.els.upstreamsHelpOverlay = new Node('div');
  ui.els.upstreamsHelpOverlay.hidden = true;
  ui.els.upstreamsHelpOverlay.appendChild(ui.els.upstreamsHelpClose);
  ui.els.upstreamsHelpOverlay.appendChild(ui.els.upstreamsHelpContent);
  ui.els.app = new Node('div');
  ui.els.breakersBody = new Node('tbody');
  ui.els.breakersMeta = new Node('span');
  ui.els.breakersEmpty = new Node('div');
  ui.els.breakersTable = new Node('table');
  for (const key of ['editorOverlay', 'confirmOverlay', 'secretOverlay']) { ui.els[key] = new Node('div'); ui.els[key].hidden = true; }
  return { ui, effects, requests, document, siteButtons, button, Node };
}

function plain(value) { return JSON.parse(JSON.stringify(value)); }
function event(key, shiftKey = false) {
  return { key, shiftKey, prevented: false, preventDefault() { this.prevented = true; } };
}

async function testUpstreamProbe() {
  const { ui, effects, requests, siteButtons, button } = loadConsole();
  const row = { id: '7' };
  const b = button('检测'); b.setAttribute('data-probe-site', '7'); siteButtons.push(b);
  const pending = ui.probeUpstreamNow(row, b);
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, '/gateway/management/probes/run');
  assert.equal(requests[0].options.method, 'POST');
  assert.deepEqual(JSON.parse(requests[0].options.body), { site_id: 7 });
  assert.equal(b.disabled, true);
  assert.equal(b.textContent, '处理中…');
  ui.probeUpstreamNow(row, b);
  assert.equal(requests.length, 1, 'same site cannot be submitted twice');
  const repainted = ui.actionCell('upstreams', row, { actions: ['detect'] }).children[0].children[0];
  assert.equal(repainted.disabled, true, 'repainted row retains pending state');
  siteButtons.push(repainted);
  requests[0].respond(200, { probes: [{ ok: true }, { ok: false }, null] });
  await pending;
  assert.equal(b.disabled, false);
  assert.equal(repainted.disabled, false);
  assert.equal(b.textContent, '检测');
  assert.match(effects.notices[0], /1 \/ 3 条线路正常/);
  assert.match(effects.notices[0], /「探测」页/);
  assert.doesNotMatch(effects.notices[0], /出现在这里|当前列表/);
  assert.equal(effects.refreshes, 1);

  for (const [status, code, word] of [[400, 'invalid_probe', '参数无效'], [404, 'unknown_site', '没有可检测的线路'], [504, 'probe_timeout', '超时']]) {
    const p = ui.probeUpstreamNow(row, b);
    requests.at(-1).respond(status, { error: { code } });
    await p;
    assert.equal(b.disabled, false);
    assert.match(effects.banners.at(-1).detail, new RegExp(word));
  }
  const auth = ui.probeUpstreamNow(row, b);
  requests.at(-1).respond(401, {}); await auth;
  assert.equal(effects.auth.length, 1); assert.equal(b.disabled, false);
  const network = ui.probeUpstreamNow(row, b);
  requests.at(-1).reject(new Error('offline')); await network;
  assert.equal(b.disabled, false); assert.equal(b.textContent, '检测');
  assert.equal(effects.banners.at(-1).detail, 'offline');
  assert.deepEqual(Object.keys(ui.state.probes.sites), []);
  const count = requests.length;
  ui.probeUpstreamNow({id: 'bad'}, b);
  assert.equal(requests.length, count);
}

async function testChannelProbeFailure() {
  const { ui, requests, effects, button } = loadConsole();
  const b = button('再探测');
  let pending = ui.probeChannelNow('line', b);
  ui.probeChannelNow('line', b);
  assert.equal(requests.length, 1);
  assert.deepEqual(JSON.parse(requests[0].options.body), {channel_id: 'line'});
  requests[0].reject(new Error('channel offline'));
  await pending;
  assert.equal(b.disabled, false);
  assert.equal(b.textContent, '再探测');
  assert.equal(effects.banners.at(-1).detail, 'channel offline');
  pending = ui.probeChannelNow('line', b);
  requests.at(-1).respond(401, {}); await pending;
  assert.equal(effects.auth.length, 1);
 }

async function testSweep() {
  const { ui, effects, requests, button } = loadConsole();
  const upstream = ui.els.upstreamsProbeAll;
  let pending = ui.runProbeSweep(upstream, '检测全部');
  assert.deepEqual(JSON.parse(requests[0].options.body), {});
  assert.equal(upstream.disabled, true);
  assert.equal(ui.els.probesRunAll.disabled, true);
  ui.runProbeSweep();
  assert.equal(requests.length, 1);
  requests[0].respond(202, { started: true }); await pending;
  assert.equal(upstream.disabled, false);
  assert.equal(upstream.textContent, '检测全部');
  assert.equal(ui.els.probesRunAll.textContent, '探测全部线路');
  assert.match(effects.notices.at(-1), /「探测」页/);
  pending = ui.runProbeSweep();
  requests.at(-1).respond(409, {}); await pending;
  assert.match(effects.notices.at(-1), /还在进行中/);
  pending = ui.runProbeSweep();
  requests.at(-1).respond(401, {}); await pending;
  assert.equal(effects.auth.length, 1);
  pending = ui.runProbeSweep();
  requests.at(-1).reject(new Error('network')); await pending;
  assert.equal(ui.state.probes.running, false);
  assert.equal(ui.els.probesRunAll.disabled, false);
  assert.equal(upstream.disabled, false);
  assert.equal(effects.banners.at(-1).detail, 'network');
  const custom = button('开始');
  pending = ui.runProbeSweep(custom, '开始');
  requests.at(-1).respond(400, {}); await pending;
  assert.equal(custom.textContent, '开始');
}

function testHelpAndEditor() {
  const { ui, document, Node } = loadConsole();
  ui.openUpstreamHelp();
  assert.equal(ui.els.upstreamsHelpOverlay.hidden, false);
  assert.equal(document.activeElement, ui.els.upstreamsHelpClose);
  assert.equal(ui.els.upstreamsHelp.getAttribute('aria-expanded'), 'true');
  for (const [name, field] of Object.entries(ui.FIELDS.upstreams)) {
    // The mapping is edited per model inside the picker, so the dialog leaves it out.
    if (field.help && name !== 'model_mapping') assert.ok(ui.els.upstreamsHelpFields.textContent.includes(field.help), name);
  }
  assert.ok(!ui.els.upstreamsHelpFields.textContent.includes(ui.FIELDS.upstreams.model_mapping.help),
    'the mapping section describes an input the form does not render');
  assert.ok(ui.els.upstreamsHelpFields.children.every(section => section.className === 'upstream-help-section'));
  assert.match(ui.els.upstreamsHelpFields.textContent, /末四位掩码/);
  assert.match(ui.els.upstreamsHelpFields.textContent, /cn:deepseek-v4\.1-flash/);
  for (const shift of [false, true]) {
    (shift ? ui.els.upstreamsHelpClose : ui.els.upstreamsHelpContent).focus();
    const tab = event('Tab', shift);
    assert.equal(ui.upstreamHelpKeydown(tab), true);
    assert.equal(tab.prevented, true);
    assert.equal(document.activeElement, shift ? ui.els.upstreamsHelpContent : ui.els.upstreamsHelpClose);
  }
  const outside = event('Tab');
  ui.els.upstreamsHelp.focus();
  ui.upstreamHelpKeydown(outside);
  assert.equal(outside.prevented, true);
  assert.equal(document.activeElement, ui.els.upstreamsHelpClose);
  const escape = event('Escape');
  ui.upstreamHelpKeydown(escape);
  assert.equal(escape.prevented, true);
  assert.equal(ui.els.upstreamsHelpOverlay.hidden, true);
  assert.equal(document.activeElement, ui.els.upstreamsHelp);
  assert.equal(ui.els.editorOverlay.hidden, true);
  assert.equal(ui.upstreamHelpKeydown(event('Escape')), false, 'other dialogs keep their keyboard handler');
  ui.openUpstreamHelp();
  ui.closeUpstreamHelp();
  assert.equal(document.activeElement, ui.els.upstreamsHelp);
  ui.els.editorOverlay.hidden = false;
  ui.openUpstreamHelp();
  assert.equal(ui.els.upstreamsHelpOverlay.hidden, true, 'help cannot overlap an existing editor');

  const upstream = ui.buildField('upstreams', { name: 'url', kind: 'text' }, { url: 'https://example.test' });
  assert.equal(upstream.wrapper.querySelectorAll('.field-help').length, 0);
  const other = ui.buildField('proxies', { name: 'url', kind: 'text' }, null);
  assert.equal(other.wrapper.querySelectorAll('.field-help').length, 1, 'other resources retain field help');
  const keys = ui.createKeysField({name: 'keys'}, null);
  assert.equal(keys.wrapper.querySelectorAll('.field-help').length, 0);
  keys.input.value = 'key-one\nkey-two';
  assert.deepEqual(plain(keys.read()), ['key-one', 'key-two']);
  const picker = ui.createModelPicker(null);
  assert.equal(picker.wrapper.querySelectorAll('.field-help').length, 0);
  assert.match(picker.wrapper.textContent, /还没有模型/);
  assert.ok(picker.wrapper instanceof Node);
}

function testBreakerHistory() {
  const { ui } = loadConsole();
  assert.equal(ui.breakerLineCell({lines: ['known', 'known']}).textContent, 'known');
  ui.state.channelNames.line = 'Current';
  assert.equal(ui.breakerLineCell({channel_id: 'line'}).textContent, 'Current');
  const named = ui.breakerLineCell({site_id: 7, site_name: '<script>name</script>', key: 'SECRET'});
  assert.equal(named.textContent, '<script>name</script>（历史线路）');
  assert.equal(named.children.length, 0, 'names are plain text');
  assert.doesNotMatch(named.textContent + named.title, /SECRET/);
  assert.match(named.title, /密钥已不再属于任何线路/);
  assert.match(named.title, /全部恢复/);
  assert.equal(ui.breakerLineCell({site_id: 7}).textContent, '上游#7（历史线路）');
  // An upstream whose models were all deselected has no line in the snapshot, so
  // the name has to come from the configuration the console already holds.
  ui.state.configuration = {resources: {upstreams: [{id: 7, name: 'Token Nest'}]}};
  assert.equal(ui.breakerLineCell({site_id: 7}).textContent, 'Token Nest（历史线路）');
  assert.equal(ui.breakerLineCell({site_id: '7'}).textContent, 'Token Nest（历史线路）');
  ui.state.configuration = null;
  assert.equal(ui.breakerLineCell({}).textContent, '未关联线路（历史记录）');
  assert.deepEqual(plain(ui.breakerResetTarget({site_id: 7, site_name: 'Only display'})), {scope: 'site', site_id: 7});
  assert.deepEqual(plain(ui.breakerResetTarget({channel_id: 'line', site_id: 7, model: 'm'})), {scope: 'channel', channel_id: 'line', model: 'm'});
  assert.equal(ui.breakerResetTarget({site_name: 'No identity'}), null);
}

// The circuit table is the reporter of the blank-column bug: every recorded
// circuit has to name something in its line column, including the ones that
// outlived their line.
function testBreakerTable() {
  const { ui } = loadConsole();
  ui.state.channelNames = {9: 'Current line'};
  ui.state.configuration = {resources: {upstreams: [{id: 1, name: 'Token Nest'}]}};
  ui.renderBreakers([
    {scope: 'channel', channel_id: '9', consecutive_failures: 0, cooldown_level: 0},
    {scope: 'key', lines: ['Token Nest'], consecutive_failures: 5, cooldown_level: 3},
    {scope: 'key', site_id: 1, site_name: 'Token Nest', consecutive_failures: 8, cooldown_level: 4},
    {scope: 'key', site_id: 2, consecutive_failures: 2, cooldown_level: 0},
    {scope: 'key', consecutive_failures: 1, cooldown_level: 0}
  ]);
  const rows = ui.els.breakersBody.children;
  assert.equal(rows.length, 5);
  const lineColumn = rows.map(row => row.children[1].textContent);
  assert.deepEqual(lineColumn, [
    'Current line',
    'Token Nest',
    'Token Nest（历史线路）',
    '上游#2（历史线路）',
    '未关联线路（历史记录）'
  ]);
  assert.ok(!lineColumn.includes('—'), 'a circuit must never render as a blank line');
  assert.equal(ui.els.breakersMeta.textContent, '跟踪 5 项');
  assert.equal(ui.els.breakersTable.hidden, false);
  assert.equal(ui.els.breakersEmpty.hidden, true);
  // Nothing recorded: the table steps aside for the empty state instead of
  // leaving an empty shell on the page.
  ui.renderBreakers([]);
  assert.equal(ui.els.breakersTable.hidden, true);
  assert.equal(ui.els.breakersEmpty.hidden, false);
  assert.equal(ui.els.breakersBody.children.length, 0);
}

(async () => {
  await testUpstreamProbe();
  await testSweep();
  await testChannelProbeFailure();
  testHelpAndEditor();
  testBreakerHistory();
  testBreakerTable();
  console.log('console behavior tests passed (probes, help/editor, breaker table, breaker history)');
})().catch(error => { console.error(error); process.exitCode = 1; });
