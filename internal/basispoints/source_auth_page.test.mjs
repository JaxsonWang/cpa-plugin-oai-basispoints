import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { setImmediate } from 'node:timers/promises';
import { test } from 'node:test';
import { runInNewContext } from 'node:vm';

const html = readFileSync(new URL('./source_auth_page.html', import.meta.url), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const storageKey = 'oai-basispoints.source-auths.management-key';
const fixtureKey = 'local-fixture-management-key';
const file = { name: 'fixture.json', auth_index: 'fixture-index', websockets: true, runtime_websockets: true };

// 只在隔离脚本中使用合成密钥和内存存储；真实浏览器另以页面重载验证。
function fixtureStorage() {
  const values = new Map();
  return {
    values,
    getItem: key => values.get(key) ?? null,
    setItem: (key, value) => { values.set(key, value); },
    removeItem: key => { values.delete(key); },
  };
}

function response(status = 200, data = { files: [file] }, nonJSON = false) {
  return { status, ok: status >= 200 && status < 300, json: async () => {
    if (nonJSON) throw new Error('fixture non-JSON');
    return data;
  } };
}

function page(storage = fixtureStorage(), handler = async () => response()) {
  const all = [];
  function element(tag) {
    const el = {
      tag, value: '', checked: false, disabled: false, textContent: '', className: '', children: [], listeners: {},
      addEventListener(name, listener) { this.listeners[name] = listener; },
      append(...children) { this.children.push(...children); },
      replaceChildren(...children) { this.children = children; },
      setAttribute(name, value) { this[name] = value; },
    };
    all.push(el);
    return el;
  }
  const elements = {};
  for (const name of ['key', 'remember', 'refresh', 'disconnect', 'connect', 'rows', 'status']) {
    const tag = html.match(new RegExp('<(\\w+)[^>]*id="' + name + '"[^>]*>'))[0];
    elements[name] = element(tag.match(/^<(\w+)/)[1]);
    elements[name].disabled = /\sdisabled(?:\s|>)/.test(tag);
    elements[name].checked = /\schecked(?:\s|>)/.test(tag);
  }
  const requests = [];
  runInNewContext(script, {
    document: {
      getElementById: id => elements[id],
      querySelectorAll: () => all.filter(el => el.tag === 'button' || el.tag === 'input'),
      createElement: element,
      createTextNode: text => ({ textContent: text }),
    },
    localStorage: storage,
    fetch: async (url, options) => { requests.push({ url, ...options }); return handler(url, options); },
  });
  return {
    elements, requests, storage,
    async submit(key = fixtureKey, remember = false) {
      elements.key.value = key;
      elements.remember.checked = remember;
      elements.connect.listeners.submit({ preventDefault() {} });
      await setImmediate();
    },
    async event(id, name = 'click') { await elements[id].listeners[name](); await setImmediate(); },
  };
}

test('default connection keeps the key in memory only', async () => {
  const p = page();
  assert.equal(p.elements.remember.checked, false);
  assert.equal(p.requests.length, 0);
  await p.submit();
  assert.equal(p.storage.values.size, 0);
  assert.equal(p.elements.key.value, '');
  assert.equal(p.elements.rows.children.length, 1);
  assert.match(p.elements.status.textContent, /仅保留在当前页面内存/);
  assert.equal(page(p.storage).requests.length, 0);
});

test('opt-in saves only after successful authentication and reconnects on reload', async () => {
  let complete;
  const p = page(fixtureStorage(), () => new Promise(resolve => { complete = resolve; }));
  await p.submit(fixtureKey, true);
  assert.equal(p.storage.values.size, 0);
  assert.equal(p.elements.remember.disabled, true);
  complete(response());
  await setImmediate();
  assert.equal(p.storage.values.get(storageKey), fixtureKey);
  const reloaded = page(p.storage);
  await setImmediate();
  assert.equal(reloaded.requests.length, 1);
  assert.equal(reloaded.requests[0].headers.Authorization, 'Bearer ' + fixtureKey);
  assert.equal(reloaded.elements.remember.checked, true);
  assert.equal(reloaded.elements.key.value, '');
  assert.equal(reloaded.elements.rows.children.length, 1);
});

test('a newly validated key replaces the remembered key', async () => {
  const p = page();
  await p.submit(fixtureKey, true);
  await p.submit('replacement-fixture-key', true);
  assert.equal(p.storage.values.get(storageKey), 'replacement-fixture-key');
});

test('remember can be enabled after login and disabled without disconnecting', async () => {
  const p = page();
  await p.submit();
  p.elements.remember.checked = true;
  await p.event('remember', 'change');
  assert.equal(p.storage.values.get(storageKey), fixtureKey);
  p.elements.remember.checked = false;
  await p.event('remember', 'change');
  assert.equal(p.storage.values.size, 0);
  await p.event('refresh');
  assert.equal(p.requests.at(-1).headers.Authorization, 'Bearer ' + fixtureKey);
  assert.equal(p.elements.rows.children.length, 1);
  assert.equal(page(p.storage).requests.length, 0);
});

test('clearing disconnects and prevents reconnecting on reload', async () => {
  const p = page();
  await p.submit(fixtureKey, true);
  await p.event('disconnect');
  assert.equal(p.storage.values.size, 0);
  assert.equal(p.elements.rows.children.length, 0);
  assert.equal(p.elements.refresh.disabled, true);
  assert.equal(p.elements.disconnect.disabled, true);
  assert.equal(p.elements.remember.checked, false);
  assert.equal(page(p.storage).requests.length, 0);
});

for (const status of [401, 403]) {
  for (const nonJSON of [false, true]) {
    test(`HTTP ${status} clears saved credentials even when nonJSON=${nonJSON}`, async () => {
      const storage = fixtureStorage();
      storage.setItem(storageKey, fixtureKey);
      const p = page(storage, async () => response(status, {}, nonJSON));
      await setImmediate();
      assert.equal(storage.values.size, 0);
      assert.equal(p.elements.refresh.disabled, true);
      assert.equal(p.elements.remember.checked, false);
      assert.match(p.elements.status.textContent, /管理鉴权失败/);
      assert.equal(page(storage).requests.length, 0);
    });
  }
}

test('an invalid manually entered key is never remembered', async () => {
  const p = page(fixtureStorage(), async () => response(401, {}));
  await p.submit('invalid-fixture-key', true);
  assert.equal(p.storage.values.size, 0);
  assert.equal(p.elements.key.value, '');
});

test('temporary upstream failure preserves an existing remembered key', async () => {
  const storage = fixtureStorage();
  storage.setItem(storageKey, fixtureKey);
  const p = page(storage, async () => response(502, { error: 'fixture upstream unavailable' }));
  await setImmediate();
  assert.equal(storage.values.get(storageKey), fixtureKey);
  assert.equal(p.requests.length, 1);
  assert.equal(p.elements.status.className, 'status error');
});

test('failed authentication requests never save a new key', async () => {
  const p = page(fixtureStorage(), async () => { throw new Error('fixture offline'); });
  await p.submit(fixtureKey, true);
  assert.equal(p.storage.values.size, 0);
  assert.equal(p.elements.rows.children.length, 0);
  assert.equal(p.elements.status.className, 'status error');
});

test('read-denied storage visibly allows a memory-only login', async () => {
  const storage = fixtureStorage();
  storage.getItem = () => { throw new Error('fixture read denied'); };
  const p = page(storage);
  assert.match(p.elements.status.textContent, /无法读取/);
  await p.submit();
  assert.equal(p.elements.rows.children.length, 1);
  assert.equal(storage.values.size, 0);
});

test('write-denied storage never reports saved or discards a valid connection', async () => {
  const storage = fixtureStorage();
  storage.setItem = () => { throw new Error('fixture quota exceeded'); };
  const p = page(storage);
  await p.submit(fixtureKey, true);
  assert.equal(p.elements.rows.children.length, 1);
  assert.equal(p.elements.refresh.disabled, false);
  assert.equal(p.elements.status.className, 'status error');
  assert.match(p.elements.status.textContent, /浏览器拒绝保存/);
  assert.doesNotMatch(p.elements.status.textContent, /管理密钥已记住/);
});

test('clear failures are reported and can be retried', async () => {
  const storage = fixtureStorage();
  const p = page(storage);
  await p.submit(fixtureKey, true);
  const remove = storage.removeItem;
  storage.removeItem = () => { throw new Error('fixture remove denied'); };
  await p.event('disconnect');
  assert.equal(p.elements.rows.children.length, 0);
  assert.equal(p.elements.refresh.disabled, true);
  assert.equal(p.elements.disconnect.disabled, false);
  assert.equal(storage.values.get(storageKey), fixtureKey);
  assert.match(p.elements.status.textContent, /无法删除/);
  storage.removeItem = remove;
  await p.event('disconnect');
  assert.equal(storage.values.size, 0);
  assert.equal(p.elements.disconnect.disabled, true);
});

test('remembered key stays in Authorization and WS edits retain the existing contract', async () => {
  const p = page(fixtureStorage(), async (_, options) => options.method === 'PATCH'
    ? response(200, { saved: true, changed: true, websockets: false }) : response());
  await p.submit(fixtureKey, true);
  const row = p.elements.rows.children[0];
  row.children[1].children[0].children[0].checked = false;
  await row.children[3].children[0].listeners.click();
  assert.deepEqual(JSON.parse(p.requests[1].body), { auth_index: 'fixture-index', websockets: false });
  assert.equal(p.requests.length, 3);
  for (const request of p.requests) {
    assert.equal(request.url, '/v0/management/oai-basispoints/source-auths');
    assert.equal(request.headers.Authorization, 'Bearer ' + fixtureKey);
    assert.equal(request.credentials, 'omit');
    assert.equal(request.cache, 'no-store');
    assert.ok(!request.url.includes(fixtureKey));
    assert.ok(!(request.body ?? '').includes(fixtureKey));
  }
  assert.ok(!p.elements.status.textContent.includes(fixtureKey));
});
