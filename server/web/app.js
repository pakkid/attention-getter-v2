// Attention Getter PWA. Vanilla JS, no build step. DOM is built with el() (never innerHTML) to avoid XSS.

const state = {
  config: null,
  me: null,
  devices: [],
  types: [],
  alerts: [],
  presets: [],
  sel: { device: localStorage.getItem('ag.device') || '', type: localStorage.getItem('ag.type') || '' },
  lastAlertIds: [],
  admin: null,
};

// ---------- helpers ----------

function el(tag, attrs = {}, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on')) n.addEventListener(k.slice(2), v);
    else if (k === 'class') n.className = v;
    else if (k in n && typeof v !== 'string') n[k] = v;
    else n.setAttribute(k, v === true ? '' : v);
  }
  for (const c of kids.flat()) if (c != null && c !== false) n.append(c instanceof Node ? c : String(c));
  return n;
}

async function api(method, path, body) {
  const opts = { method, headers: { 'X-AG-CSRF': '1' }, credentials: 'same-origin' };
  if (body instanceof FormData) opts.body = body;
  else if (body !== undefined) { opts.body = JSON.stringify(body); opts.headers['Content-Type'] = 'application/json'; }
  const r = await fetch(path, opts);
  if (r.status === 401 && path !== '/api/me') { state.me = null; render(); throw new Error('Signed out'); }
  const text = await r.text();
  const data = text ? JSON.parse(text) : null;
  if (!r.ok) throw new Error(data?.error || r.statusText);
  return data;
}

let toastTimer;
function toast(msg) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove('show'), 3000);
}

const guard = (fn) => async (...a) => { try { await fn(...a); } catch (e) { toast(e.message); } };

function ago(ts) {
  if (!ts) return 'never';
  const s = Math.max(0, Date.now() / 1000 - ts);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return new Date(ts * 1000).toLocaleDateString();
}

function duration(a) {
  if (!a.resolved) return '';
  const s = a.resolved - a.created;
  return s < 60 ? `${s}s` : `${Math.round(s / 60)} min`;
}

const mediaURL = (t, kind) => `/media/${t.id}/${kind}?h=${t.hash}`;
const isStandalone = () => matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
const isIOS = () => /iphone|ipad|ipod/i.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);

// ---------- boot ----------

async function boot() {
  if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {});
  state.config = await api('GET', '/api/config');
  try { state.me = await api('GET', '/api/me'); } catch { state.me = null; }
  if (state.me) await loadAll();
  window.addEventListener('hashchange', render);
  document.addEventListener('visibilitychange', () => { if (!document.hidden && state.me) loadAll().then(render); });
  render();
}

async function loadAll() {
  const [devices, types, alerts, presets] = await Promise.all([
    api('GET', '/api/devices'), api('GET', '/api/types'), api('GET', '/api/alerts?limit=50'), api('GET', '/api/presets'),
  ]);
  Object.assign(state, { devices, types, alerts, presets });
  connectEvents();
}

let es;
function connectEvents() {
  if (es && es.readyState !== EventSource.CLOSED) return;
  es = new EventSource('/api/events');
  es.onmessage = async (e) => {
    const ev = JSON.parse(e.data);
    if (ev.type === 'alert') {
      const i = state.alerts.findIndex((a) => a.id === ev.alert.id);
      if (i >= 0) state.alerts[i] = ev.alert; else state.alerts.unshift(ev.alert);
    } else if (ev.type === 'devices') {
      state.devices = await api('GET', '/api/devices');
    }
    softRender();
  };
}

// Re-render without clobbering what the user is typing.
function softRender() {
  const a = document.activeElement;
  if (a && (a.tagName === 'INPUT' || a.tagName === 'TEXTAREA' || a.tagName === 'SELECT')) {
    const live = document.getElementById('live');
    if (live && tab() === 'trigger') live.replaceWith(liveStatus());
    return;
  }
  render();
}

const tab = () => (location.hash.slice(1) || 'trigger');

function render() {
  const app = document.getElementById('app');
  const top = document.getElementById('top');
  if (!state.me) {
    top.hidden = true;
    app.replaceChildren(loginView());
    return;
  }
  top.hidden = false;
  const t = tab();
  document.querySelector('[data-tab=admin]').hidden = state.me.role !== 'admin';
  for (const a of document.querySelectorAll('#tabs a')) a.classList.toggle('active', a.dataset.tab === t);
  const view = { trigger: triggerView, history: historyView, settings: settingsView, admin: adminView }[t] || triggerView;
  app.replaceChildren(view());
}

// ---------- login ----------

function loginView() {
  const btn = el('div', { id: 'gsi' });
  const wrap = el('div', { class: 'login' },
    el('img', { src: '/icons/icon-192.png', alt: '' }),
    el('h1', {}, 'Attention Getter'),
    btn,
  );
  const init = () => {
    if (!window.google?.accounts?.id) return setTimeout(init, 200);
    if (!state.config.google_client_id) return;
    google.accounts.id.initialize({
      client_id: state.config.google_client_id,
      callback: guard(async (r) => {
        state.me = await api('POST', '/auth/google', { credential: r.credential });
        await loadAll();
        render();
      }),
    });
    google.accounts.id.renderButton(btn, { theme: 'filled_black', size: 'large', shape: 'pill', text: 'signin_with' });
  };
  init();
  if (!state.config.google_client_id) wrap.append(el('p', { class: 'muted small' }, 'GOOGLE_CLIENT_ID is not configured on the server.'));
  if (state.config.dev_login) {
    const email = el('input', { type: 'email', placeholder: 'email (dev login)' });
    wrap.append(el('div', { class: 'row' }, email, el('button', {
      onclick: guard(async () => {
        state.me = await api('POST', '/auth/dev', { email: email.value });
        await loadAll();
        render();
      }),
    }, 'Dev sign-in')));
  }
  return wrap;
}

// ---------- trigger ----------

function triggerView() {
  const v = el('div');
  if (!state.devices.length) {
    v.append(el('div', { class: 'card' }, el('p', {}, 'No PCs are paired yet.'),
      state.me.role === 'admin' ? el('p', { class: 'muted small' }, 'Go to Admin → PCs → Add PC.') : el('p', { class: 'muted small' }, 'Ask an admin to pair a PC.')));
    return v;
  }
  if (!state.devices.some((d) => d.name === state.sel.device) && state.sel.device !== 'all') state.sel.device = state.devices[0].name;
  if (state.types.length && !state.types.some((t) => String(t.id) === state.sel.type)) state.sel.type = String(state.types[0].id);

  const pick = (key, val) => { state.sel[key] = val; localStorage.setItem('ag.' + key, val); render(); };

  v.append(el('h2', {}, 'PC'));
  const chips = el('div', { class: 'chips' });
  for (const d of state.devices) {
    chips.append(el('button', { class: 'chip', 'aria-pressed': String(state.sel.device === d.name), onclick: () => pick('device', d.name) },
      el('span', { class: 'dot' + (d.online ? ' on' : ''), title: d.online ? 'online' : 'offline' }), d.name));
  }
  if (state.devices.length > 1) {
    chips.append(el('button', { class: 'chip', 'aria-pressed': String(state.sel.device === 'all'), onclick: () => pick('device', 'all') }, 'All PCs'));
  }
  v.append(chips);

  if (state.types.length) {
    v.append(el('h2', {}, 'Type'));
    const grid = el('div', { class: 'types' });
    for (const t of state.types) {
      grid.append(el('button', { class: 'type', 'aria-pressed': String(state.sel.type === String(t.id)), onclick: () => pick('type', String(t.id)) },
        el('img', { src: mediaURL(t, 'gif'), alt: '', loading: 'lazy' }), el('span', {}, t.name)));
    }
    v.append(grid);
  }

  v.append(el('h2', {}, 'Message (optional)'));
  const msg = el('input', { type: 'text', maxlength: '200', placeholder: 'e.g. dinner is ready', enterkeyhint: 'send' });
  const send = el('button', { class: 'primary big' }, 'Get their attention');
  const go = guard(async () => {
    send.disabled = true;
    try {
      const res = await api('POST', '/api/alerts', { device: state.sel.device, type: state.sel.type, message: msg.value });
      state.lastAlertIds = res.map((r) => r.alert_id);
      msg.value = '';
      const off = res.filter((r) => !r.online).map((r) => r.device);
      toast(off.length ? `${off.join(', ')} offline, will show when it reconnects` : res.some((r) => r.merged) ? 'Added to the open alert' : 'Sent!');
      state.alerts = await api('GET', '/api/alerts?limit=50');
      render();
    } finally { send.disabled = false; }
  });
  msg.addEventListener('keydown', (e) => { if (e.key === 'Enter') go(); });
  send.addEventListener('click', go);
  v.append(msg, el('div', { style: 'height:14px' }), send, liveStatus());
  return v;
}

function liveStatus() {
  const box = el('div', { id: 'live', style: 'margin-top:16px' });
  const mine = state.alerts.filter((a) => state.lastAlertIds.includes(a.id) || (a.status === 'pending' || a.status === 'delivered'));
  for (const a of mine.slice(0, 4)) box.append(alertCard(a, true));
  return box;
}

const statusText = {
  pending: 'Waiting for the PC…',
  delivered: 'Showing on screen',
  replied: 'Replied',
  dismissed: 'Dismissed without a reply',
  cancelled: 'Cancelled',
};

function alertCard(a, withCancel) {
  const online = state.devices.find((d) => d.id === a.device_id)?.online;
  const who = a.requests.map((r) => r.name).join(', ');
  const msgs = a.requests.filter((r) => r.message).map((r) => `${r.name}: “${r.message}”`);
  const c = el('div', { class: `card status ${a.status}` },
    el('div', { class: 'row' },
      el('div', { class: 'grow' }, el('strong', {}, a.device_name), a.type_name ? el('span', { class: 'muted' }, ` · ${a.type_name}`) : null),
      el('span', { class: 'muted small' }, ago(a.created))),
    a.status === 'replied' ? el('div', { class: 'reply' }, a.reply) : el('div', { class: 'muted' },
      a.status === 'pending' && !online ? 'PC offline, will show when it reconnects' : statusText[a.status]),
    el('div', { class: 'muted small' }, `by ${who}`, duration(a) ? ` · answered in ${duration(a)}` : ''),
    msgs.length ? el('div', { class: 'small' }, msgs.join(' · ')) : null,
  );
  if (withCancel && (a.status === 'pending' || a.status === 'delivered')) {
    c.append(el('div', { style: 'margin-top:8px' }, el('button', {
      class: 'link', onclick: guard(async () => { await api('POST', `/api/alerts/${a.id}/cancel`); }),
    }, 'Cancel')));
  }
  return c;
}

// ---------- history ----------

function historyView() {
  const v = el('div', {}, el('h2', {}, 'Recent'));
  if (!state.alerts.length) v.append(el('p', { class: 'muted' }, 'Nothing yet.'));
  for (const a of state.alerts) v.append(alertCard(a, true));
  return v;
}

// ---------- settings ----------

function settingsView() {
  const v = el('div');
  v.append(el('h2', {}, 'Account'), el('div', { class: 'card row' },
    state.me.picture ? el('img', { src: state.me.picture, alt: '', referrerpolicy: 'no-referrer', style: 'width:40px;height:40px;border-radius:50%' }) : null,
    el('div', { class: 'grow' }, el('div', {}, state.me.name || state.me.email), el('div', { class: 'muted small' }, state.me.email)),
    el('button', { onclick: guard(async () => { await api('POST', '/auth/logout'); state.me = null; es?.close(); render(); }) }, 'Sign out')));

  v.append(el('h2', {}, 'Notifications'));
  const card = el('div', { class: 'card stack' });
  v.append(card);
  const pushSupported = 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
  if (isIOS() && !isStandalone()) {
    card.append(el('p', {}, 'To get notifications on iPhone/iPad: tap the Share button, then “Add to Home Screen”, and open the app from your home screen.'),
      el('p', { class: 'muted small' }, 'Requires iOS 16.4 or later.'));
  } else if (!pushSupported) {
    card.append(el('p', { class: 'muted' }, 'This browser does not support push notifications.'));
  } else {
    const line = el('div', { class: 'row' }, el('div', { class: 'grow' }, 'Checking…'));
    card.append(line);
    pushState().then((sub) => {
      const perm = Notification.permission;
      line.replaceChildren(
        el('div', { class: 'grow' }, sub ? 'Enabled on this device' : perm === 'denied' ? 'Blocked in browser settings' : 'Off on this device'),
        sub ? el('button', { onclick: guard(async () => { await disablePush(sub); render(); }) }, 'Turn off')
          : el('button', { class: 'primary', disabled: perm === 'denied', onclick: guard(async () => { await enablePush(); render(); }) }, 'Enable'),
      );
    });
  }

  const prefs = [['mine', 'Replies to my requests'], ['all', 'Every reply'], ['none', 'Nothing']];
  card.append(el('div', {}, el('label', { class: 'field' }, 'Notify me about'),
    el('div', { class: 'chips' }, prefs.map(([val, label]) => el('button', {
      class: 'chip', 'aria-pressed': String(state.me.notify_pref === val),
      onclick: guard(async () => { state.me = await api('PATCH', '/api/me', { notify_pref: val }); render(); }),
    }, label)))));
  return v;
}

async function pushState() {
  const reg = await navigator.serviceWorker.ready;
  return reg.pushManager.getSubscription();
}

function b64urlToBytes(s) {
  const pad = '='.repeat((4 - (s.length % 4)) % 4);
  const raw = atob((s + pad).replace(/-/g, '+').replace(/_/g, '/'));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
}

async function enablePush() {
  // Must run inside the tap handler: iOS only shows the prompt for a user gesture.
  const perm = await Notification.requestPermission();
  if (perm !== 'granted') throw new Error('Notifications were not allowed');
  const reg = await navigator.serviceWorker.ready;
  const sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64urlToBytes(state.config.vapid_public_key) });
  await api('POST', '/api/push/subscribe', sub.toJSON());
  toast('Notifications on');
}

async function disablePush(sub) {
  await api('POST', '/api/push/unsubscribe', { endpoint: sub.endpoint });
  await sub.unsubscribe();
}

// ---------- admin ----------

function adminView() {
  const v = el('div');
  if (!state.admin) {
    Promise.all([api('GET', '/api/admin/users'), api('GET', '/api/admin/keys')]).then(([users, keys]) => {
      state.admin = { users, keys };
      render();
    }).catch((e) => toast(e.message));
    v.append(el('p', { class: 'muted' }, 'Loading…'));
    return v;
  }
  const reload = async () => {
    const [users, keys, devices, types, presets] = await Promise.all([
      api('GET', '/api/admin/users'), api('GET', '/api/admin/keys'), api('GET', '/api/devices'), api('GET', '/api/types'), api('GET', '/api/presets'),
    ]);
    Object.assign(state, { devices, types, presets });
    state.admin = { ...state.admin, users, keys };
    render();
  };
  v.append(pcsSection(reload), typesSection(reload), keysSection(reload), usersSection(reload), presetsSection(reload));
  return v;
}

function pcsSection(reload) {
  const s = el('div', {}, el('h2', {}, 'PCs'));
  const list = el('div', { class: 'card list' });
  for (const d of state.devices) {
    list.append(el('div', { class: 'item' }, el('span', { class: 'dot' + (d.online ? ' on' : '') }),
      el('div', { class: 'grow' }, el('div', {}, d.name), el('div', { class: 'muted small' }, d.online ? 'online' : `last seen ${ago(d.last_seen)}`)),
      el('button', { class: 'link danger', onclick: guard(async () => {
        if (!confirm(`Remove ${d.name}? It will need to be paired again.`)) return;
        await api('DELETE', `/api/admin/devices/${d.id}`); await reload();
      }) }, 'Remove')));
  }
  const pair = el('div', { class: 'item' });
  pair.append(el('button', { onclick: guard(async () => {
    const { code } = await api('POST', '/api/admin/pairing');
    pair.replaceChildren(el('div', { class: 'stack grow' },
      el('div', { class: 'code' }, code),
      el('div', { class: 'muted small' }, 'On the PC run ', el('span', { class: 'mono' }, 'attention-getter setup'), ` and enter this server URL and code. Expires in 15 minutes.`),
      el('div', { class: 'mono secret' }, location.origin)));
  }) }, '+ Add PC'));
  list.append(pair);
  s.append(list);
  return s;
}

function typesSection(reload) {
  const s = el('div', {}, el('h2', {}, 'Attention types'));
  const list = el('div', { class: 'card list' });
  for (const t of state.types) {
    const audio = el('audio', { src: mediaURL(t, 'sound'), preload: 'none' });
    list.append(el('div', { class: 'item' }, el('img', { src: mediaURL(t, 'gif'), alt: '', loading: 'lazy' }),
      el('div', { class: 'grow' }, t.name, audio),
      el('button', { class: 'link', onclick: () => (audio.paused ? audio.play() : audio.pause()) }, '▶︎'),
      el('button', { class: 'link', onclick: () => list.replaceWith(typeForm(t, reload)) }, 'Edit'),
      el('button', { class: 'link danger', onclick: guard(async () => {
        if (!confirm(`Delete type ${t.name}?`)) return;
        await api('DELETE', `/api/admin/types/${t.id}`); await reload();
      }) }, 'Delete')));
  }
  list.append(el('div', { class: 'item' }, el('button', { onclick: () => list.replaceWith(typeForm(null, reload)) }, '+ Add type')));
  s.append(list);
  return s;
}

function typeForm(t, reload) {
  const name = el('input', { type: 'text', value: t?.name || '', maxlength: '40', placeholder: 'e.g. dinner' });
  const gif = el('input', { type: 'file', accept: 'image/gif' });
  const sound = el('input', { type: 'file', accept: 'audio/mpeg,audio/ogg,audio/wav,audio/flac,.mp3,.ogg,.wav,.flac' });
  const save = el('button', { class: 'primary' }, t ? 'Save' : 'Create');
  save.addEventListener('click', guard(async () => {
    const fd = new FormData();
    fd.append('name', name.value);
    if (gif.files[0]) fd.append('gif', gif.files[0]);
    if (sound.files[0]) fd.append('sound', sound.files[0]);
    save.disabled = true;
    try {
      await api(t ? 'PUT' : 'POST', t ? `/api/admin/types/${t.id}` : '/api/admin/types', fd);
      toast('Saved; PCs will sync it now');
      await reload();
    } finally { save.disabled = false; }
  }));
  return el('div', { class: 'card stack' },
    el('div', {}, el('label', { class: 'field' }, 'Name'), name),
    el('div', {}, el('label', { class: 'field' }, t ? 'GIF (leave empty to keep)' : 'GIF (max 20 MB)'), gif),
    el('div', {}, el('label', { class: 'field' }, t ? 'Sound (leave empty to keep)' : 'Sound: mp3, ogg, wav or flac (max 5 MB)'), sound),
    el('div', { class: 'row' }, save, el('button', { onclick: () => render() }, 'Cancel')));
}

function keysSection(reload) {
  const s = el('div', {}, el('h2', {}, 'Trigger keys (Alexa etc.)'));
  const list = el('div', { class: 'card list' });
  const devName = (id) => state.devices.find((d) => d.id === id)?.name;
  const typeName = (id) => state.types.find((t) => t.id === id)?.name;
  for (const k of state.admin.keys) {
    const pins = [k.pinned_device && `PC: ${devName(k.pinned_device)}`, k.pinned_type && `type: ${typeName(k.pinned_type)}`].filter(Boolean).join(', ');
    list.append(el('div', { class: 'item' },
      el('div', { class: 'grow' }, el('div', {}, k.name), el('div', { class: 'muted small' }, [pins, `used ${ago(k.last_used)}`].filter(Boolean).join(' · '))),
      el('button', { class: 'link danger', onclick: guard(async () => {
        if (!confirm(`Revoke key ${k.name}?`)) return;
        await api('DELETE', `/api/admin/keys/${k.id}`); await reload();
      }) }, 'Revoke')));
  }
  const name = el('input', { type: 'text', placeholder: 'Name shown in popup, e.g. Alexa' });
  const pc = el('select', {}, el('option', { value: '' }, 'Any PC (use ?pc=)'), state.devices.map((d) => el('option', { value: d.id }, d.name)));
  const type = el('select', {}, el('option', { value: '' }, 'Any type (use ?type=)'), state.types.map((t) => el('option', { value: t.id }, t.name)));
  const out = el('div');
  const create = el('button', { class: 'primary', onclick: guard(async () => {
    const { key } = await api('POST', '/api/admin/keys', {
      name: name.value, pinned_device: pc.value ? Number(pc.value) : null, pinned_type: type.value ? Number(type.value) : null,
    });
    const url = `${location.origin}/api/trigger?key=${key}`;
    state.admin.keys = await api('GET', '/api/admin/keys');
    out.replaceChildren(el('div', { class: 'stack' },
      el('div', { class: 'small' }, 'POST to this URL. It is shown only once, so copy it now. Optional params: &pc=NAME|all &type=NAME &message=TEXT'),
      el('div', { class: 'mono secret' }, url),
      el('button', { onclick: () => navigator.clipboard.writeText(url).then(() => toast('Copied')) }, 'Copy URL')));
  }) }, 'Create key');
  list.append(el('div', { class: 'item' }, el('div', { class: 'stack grow' }, name, pc, type, create, out)));
  s.append(list);
  return s;
}

function usersSection(reload) {
  const s = el('div', {}, el('h2', {}, 'Allowed Google accounts'));
  const list = el('div', { class: 'card list' });
  for (const u of state.admin.users) {
    const self = u.email === state.me.email;
    list.append(el('div', { class: 'item' },
      el('div', { class: 'grow' }, el('div', {}, u.name || u.email), el('div', { class: 'muted small' }, u.email)),
      el('span', { class: 'badge' + (u.role === 'admin' ? ' warn' : '') }, u.role),
      self ? null : el('button', { class: 'link', onclick: guard(async () => {
        await api('POST', '/api/admin/users', { email: u.email, role: u.role === 'admin' ? 'user' : 'admin' }); await reload();
      }) }, u.role === 'admin' ? 'Make user' : 'Make admin'),
      self ? null : el('button', { class: 'link danger', onclick: guard(async () => {
        if (!confirm(`Remove ${u.email}?`)) return;
        await api('DELETE', `/api/admin/users/${encodeURIComponent(u.email)}`); await reload();
      }) }, 'Remove')));
  }
  const email = el('input', { type: 'email', placeholder: 'name@gmail.com' });
  list.append(el('div', { class: 'item' }, el('div', { class: 'grow' }, email), el('button', { onclick: guard(async () => {
    await api('POST', '/api/admin/users', { email: email.value, role: 'user' }); await reload();
  }) }, 'Add')));
  s.append(list);
  return s;
}

function presetsSection(reload) {
  const ta = el('textarea', { placeholder: 'One per line (max 9)' });
  ta.value = state.presets.join('\n');
  return el('div', {}, el('h2', {}, 'Quick replies (keys 1–9 in the popup)'), el('div', { class: 'card stack' }, ta,
    el('button', { onclick: guard(async () => {
      await api('PUT', '/api/admin/presets', ta.value.split('\n'));
      toast('Saved'); await reload();
    }) }, 'Save')));
}

boot().catch((e) => {
  document.getElementById('app').replaceChildren(el('p', { class: 'center muted' }, `Failed to load: ${e.message}`));
});
