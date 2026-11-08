const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const html = fs.readFileSync(require('node:path').join(__dirname, '../internal/kiro/connect_page.html'), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const future = () => new Date(Date.now() + 600000).toISOString();
const pending = (extra = {}) => ({status: 'pending', method: 'social', expiresAt: future(), url: 'https://app.kiro.dev/signin?state=test', ...extra});
const response = (data, status = 200) => ({ok: status < 400, status, json: async () => data});
const settle = () => new Promise(setImmediate);

function page(fetcher, saved = true) {
  const elements = new Map();
  for (const match of html.matchAll(/<([a-z]+)\b([^>]*\bid="([^"]+)"[^>]*)>/g)) {
    const [, tag, attrs, id] = match;
    const classes = new Set((attrs.match(/class="([^"]*)"/)?.[1] || '').split(/\s+/).filter(Boolean));
    const handlers = {};
    elements.set(id, {id, tag, value: attrs.match(/value="([^"]*)"/)?.[1] || (tag === 'select' ? 'social' : ''),
      textContent: '', disabled: false, style: {}, dataset: {}, handlers,
      classList: {add: c => classes.add(c), remove: c => classes.delete(c), contains: c => classes.has(c),
        toggle(c, on) { const show = on === undefined ? !classes.has(c) : on; show ? classes.add(c) : classes.delete(c); }},
      setAttribute(name, value) {this[name] = value;}, addEventListener(name, fn) {handlers[name] = fn;}});
  }
  const methods = ['builder', 'social', 'idc', 'import'].map(pane => ({dataset: {pane}, disabled: false,
    setAttribute(name, value) {this[name] = value;}, addEventListener() {}}));
  let timerID = 0, calls = [];
  const timers = new Map();
  const get = id => {assert.ok(elements.has(id), `missing DOM element ${id}`); return elements.get(id);};
  const context = {URL, URLSearchParams, TextEncoder, TextDecoder, Uint8Array, Date, Number,
    location: {hash: '#state=example', host: 'example.test', origin: 'https://example.test', href: 'https://example.test/connect'},
    navigator: {userAgent: 'test', clipboard: {writeText: async () => {}}}, window: {},
    localStorage: {getItem(key) {return saved && key === 'cli-proxy-auth' ? JSON.stringify({state: {managementKey: 'test-key'}}) : null;}},
    document: {getElementById: get, querySelectorAll(selector) {
      if (selector === '.method') return methods;
      if (selector === '.pane') return ['builder', 'social', 'idc', 'import'].map(p => get('pane-' + p));
      if (selector === '.pane button') return [...elements.values()].filter(n => n.tag === 'button' && /^(btn-(builder|idc|social|import)|copy-)/.test(n.id));
      throw new Error('unexpected selector ' + selector);
    }},
    setTimeout(fn, ms) {const id = ++timerID; timers.set(id, {fn, ms, once: true}); return id;},
    clearTimeout(id) {timers.delete(id);},
    setInterval(fn, ms) {const id = ++timerID; timers.set(id, {fn, ms, once: false}); return id;},
    clearInterval(id) {timers.delete(id);},
    async fetch(url, opts) {calls.push({url, opts}); return fetcher(calls.length, url, opts);}
  };
  vm.runInNewContext(script, context);
  return {get, methods, calls, timers,
    async click(id) {await get(id).handlers.click.call(get(id)); await settle();},
    async tick() {const entry = [...timers].find(([, v]) => v.once); assert.ok(entry, 'poll scheduled'); timers.delete(entry[0]); await entry[1].fn(); await settle();},
    polls() {return [...timers.values()].filter(v => v.once).length;}};
}

test('a failed manual key stops polling until explicit correction', async () => {
  const p = page(n => response(n === 2 ? {} : pending(), n === 2 ? 401 : 200), false);
  assert.equal(p.calls.length, 0);
  p.get('mgmt-key').value = 'bad-key'; await p.click('btn-key-resume'); await p.tick();
  assert.equal(p.calls.length, 2); assert.equal(p.polls(), 0); assert.equal(p.get('mgmt-key').value, '');
  assert.match(p.get('msg').textContent, /密钥无效/);
  await p.click('btn-social-complete'); assert.equal(p.calls.length, 2);
  p.get('mgmt-key').value = 'corrected'; await p.click('btn-key-resume');
  assert.equal(p.calls.length, 3); assert.equal(p.polls(), 1);
});
test('403 pauses with an access restriction explanation', async () => {
  const p = page(() => response({}, 403)); await settle();
  assert.equal(p.polls(), 0); assert.match(p.get('msg').textContent, /访问限制/);
  assert.equal(p.get('key-box').style.display, 'block');
});
test('reload restores social login pane and callback entry', async () => {
  const p = page(() => response(pending())); await settle();
  assert.equal(p.get('pane-social').classList.contains('active'), true);
  assert.equal(p.get('social-paste').classList.contains('hidden'), false);
  assert.match(p.get('link-social').href, /^https:\/\/app.kiro.dev/);
  assert.ok(p.methods.every(b => b.disabled)); assert.equal(p.polls(), 1);
});
test('reload restores device authorization and countdown', async () => {
  const p = page(() => response(pending({method: 'device-idc', verificationUri: 'https://view.awsapps.com/start', userCode: 'ABCD-EFGH', deviceExpiresAt: future(), region: 'eu-central-1', startUrl: 'https://d-test.awsapps.com/start'})));
  await settle();
  assert.equal(p.get('code-idc').textContent, 'ABCD-EFGH'); assert.equal(p.get('btn-idc').disabled, true);
  assert.equal(p.get('device-idc').classList.contains('hidden'), false);
  assert.equal(p.get('idc-region').value, 'eu-central-1'); assert.match(p.get('countdown').textContent, /剩余时间/);
});
test('three consecutive network failures stop rather than spin silently', async () => {
  const p = page(n => {if (n > 1) throw new Error('offline'); return response(pending());});
  await settle(); for (let i = 0; i < 3; i++) await p.tick();
  assert.equal(p.calls.length, 4); assert.equal(p.polls(), 0);
  assert.match(p.get('msg').textContent, /offline/); assert.equal(p.get('btn-resume').classList.contains('hidden'), false);
});
test('slow poll cannot overlap another poll or a login action', async () => {
  let release;
  const p = page(n => n === 1 ? response(pending()) : new Promise(resolve => {release = resolve;}));
  await settle(); const tick = p.tick(); await settle();
  assert.equal(p.polls(), 0); await p.click('btn-builder'); assert.equal(p.calls.length, 2);
  release(response(pending())); await tick; assert.equal(p.polls(), 1);
});
test('terminal device failure offers a fresh panel login', async () => {
  const p = page(() => response({status: 'error', error: 'AWS 授权超时', method: 'device-builder'})); await settle();
  assert.equal(p.polls(), 0); assert.equal(p.get('btn-restart').classList.contains('hidden'), false);
  assert.match(p.get('msg').textContent, /重新发起/);
});
test('invalid imported credentials show a warning and do not claim saved success', async () => {
  const p = page(() => response({status: 'success', validation: 'invalid', warning: '凭据已失效', delivered: false})); await settle();
  assert.equal(p.get('msg').className, 'msg warn'); assert.match(p.get('msg').textContent, /凭据已失效/);
  assert.match(p.get('msg').textContent, /等待面板保存/); assert.equal(p.polls(), 0);
});
test('expired session stops all future requests', async () => {
  const p = page(() => response({error: '登录会话不存在或已过期'}, 404)); await settle();
  assert.equal(p.polls(), 0); assert.ok(p.methods.every(b => b.disabled));
});
