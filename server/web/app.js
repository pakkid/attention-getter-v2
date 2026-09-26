// Attention Getter PWA. Vanilla JS, no build step. DOM is built with el() (never innerHTML) to avoid XSS.

const state = {
  config: null,
  me: null,
  devices: [],
  groups: [],
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
  if (!a.resolved || (a.status !== 'replied' && a.status !== 'dismissed')) return '';
  const s = a.resolved - a.created;
  return s < 60 ? `${s}s` : `${Math.round(s / 60)} min`;
}

const mediaURL = (t, kind) => `/media/${t.id}/${kind}?h=${t.hash}`;
const shownName = (u) => u.display_name || u.name || u.email;
const firstName = (n) => (n.includes('@') ? n.split('@')[0] : n.split(/\s+/)[0]);

// Broadsheet line icons: 24px grid, 1.5 stroke, round caps, drawn in currentColor.
const ICONS = {
  check: ['M5 12.5l4.5 4.5L19 7.5'],
};
function icon(name, size = 18) {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  for (const [k, v] of Object.entries({ class: 'icon', width: size, height: size, viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor',
    'stroke-width': 1.5, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true' })) svg.setAttribute(k, v);
  for (const d of ICONS[name]) { const p = document.createElementNS(ns, 'path'); p.setAttribute('d', d); svg.append(p); }
  return svg;
}

const tag = (tone, text) => el('span', { class: `tag tag-${tone}` }, tone === 'live' ? el('span', { class: 'tag-dot', 'aria-hidden': 'true' }) : null, text);
const dot = (on) => [el('span', { class: 'dot' + (on ? ' on' : ''), 'aria-hidden': 'true' }), el('span', { class: 'sr-only' }, on ? 'online, ' : 'offline, ')];

function copyBtn(text) {
  return el('button', { class: 'link', onclick: () => navigator.clipboard.writeText(text).then(() => toast('Copied'), () => toast('Copy failed')) }, 'Copy');
}

// ---------- motion ----------

const calm = () => matchMedia('(prefers-reduced-motion: reduce)').matches;
// One-shot animation cues, consumed by the next render so ordinary re-renders stay still.
const fx = { picked: '', fresh: new Set(), changed: new Set() };

// Replays a CSS animation class on an element.
function play(node, cls, ms = 900) {
  if (!node || calm()) return;
  node.classList.remove(cls);
  void node.offsetWidth;
  node.classList.add(cls);
  setTimeout(() => node.classList.remove(cls), ms);
}

// A sent alert rings out: the send button shakes like an alarm clock, red rings pulse outward and the phone buzzes.
function alarm() {
  const btn = document.querySelector('button.big');
  if (!btn || calm()) return;
  navigator.vibrate?.([70, 50, 70, 50, 70]);
  play(btn, 'alarm', 800);
  const r = btn.getBoundingClientRect();
  for (let i = 0; i < 3; i++) {
    const ring = el('span', { class: 'alarm-ring', 'aria-hidden': 'true' });
    Object.assign(ring.style, { left: `${r.left}px`, top: `${r.top}px`, width: `${r.width}px`, height: `${r.height}px` });
    document.body.append(ring);
    ring.animate([
      { outlineOffset: '0px', opacity: 0.8 },
      { outlineOffset: '26px', opacity: 0 },
    ], { duration: 750, delay: i * 180, easing: 'cubic-bezier(.2,.7,.3,1)', fill: 'backwards' }).finished.then(() => ring.remove());
  }
}

// Nothing went out: a quick side-to-side "nope".
function nope() {
  play(document.querySelector('button.big'), 'nope', 600);
  if (!calm()) navigator.vibrate?.([40, 60, 40]);
}

// Slides the tab underline under the current tab.
function placeIndicator() {
  const bar = document.querySelector('.tab-indicator');
  const a = document.querySelector('#tabs a[aria-current=page]');
  if (!bar || !a || !a.offsetWidth) return;
  bar.style.width = `${a.offsetWidth}px`;
  bar.style.transform = `translateX(${a.offsetLeft}px)`;
  requestAnimationFrame(() => bar.classList.add('ready'));
}

// Staggered rise of the page's pieces; used on first load and when view transitions are unavailable.
function enterPage() {
  const app = document.getElementById('app');
  if (calm()) return;
  play(app, 'enter', 1200);
}

const TABS = ['trigger', 'history', 'settings', 'admin'];
let shownTab = null;

// Tab change: the old page slides out and the new one bounces in from the side you're heading to.
function changeTab() {
  const from = TABS.indexOf(shownTab), to = TABS.indexOf(tab());
  const swap = () => { scrollTo(0, 0); render(); };
  if (!state.me || calm() || !document.startViewTransition) { swap(); enterPage(); return; }
  const root = document.documentElement;
  root.dataset.dir = to < from ? 'back' : 'forward';
  root.classList.add('vt-running');
  document.startViewTransition(swap).finished.finally(() => root.classList.remove('vt-running'));
}

const isStandalone = () => matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
const isIOS = () => /iphone|ipad|ipod/i.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);

// ---------- boot ----------

async function boot() {
  if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {});
  state.config = await api('GET', '/api/config');
  try { state.me = await api('GET', '/api/me'); } catch { state.me = null; }
  if (state.me) await loadAll();
  window.addEventListener('hashchange', changeTab);
  window.addEventListener('resize', placeIndicator);
  document.addEventListener('visibilitychange', () => { if (!document.hidden && state.me) loadAll().then(render); });
  render();
  if (!calm()) play(document.getElementById('top'), 'drop', 1000);
  enterPage();
}

async function loadAll() {
  const [devices, groups, types, alerts, presets] = await Promise.all([
    api('GET', '/api/devices'), api('GET', '/api/groups'), api('GET', '/api/types'), api('GET', '/api/alerts?limit=50'), api('GET', '/api/presets'),
  ]);
  Object.assign(state, { devices, groups, types, alerts, presets });
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
      if (i >= 0) {
        if (state.alerts[i].status !== ev.alert.status) fx.changed.add(ev.alert.id);
        state.alerts[i] = ev.alert;
      } else {
        fx.fresh.add(ev.alert.id);
        state.alerts.unshift(ev.alert);
      }
    } else if (ev.type === 'devices') {
      [state.devices, state.groups] = await Promise.all([api('GET', '/api/devices'), api('GET', '/api/groups')]);
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
  for (const a of document.querySelectorAll('#tabs a')) {
    if (a.dataset.tab === t) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  }
  placeIndicator();
  shownTab = t;
  const view = { trigger: triggerView, history: historyView, settings: settingsView, admin: adminView }[t] || triggerView;
  app.replaceChildren(view());
  fx.picked = '';
}

// ---------- login ----------

function loginView() {
  const btn = el('div', { id: 'gsi' });
  const actions = el('div', { class: 'actions' }, btn);
  const wrap = el('div', { class: 'login' },
    el('img', { src: '/icons/icon-192.png', alt: '', class: 'bell', onclick: (e) => play(e.currentTarget, 'ring', 1100) }),
    el('h1', {}, 'Attention Getter'),
    actions,
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
        play(document.getElementById('top'), 'drop', 1000);
        enterPage();
      }),
    });
    google.accounts.id.renderButton(btn, { theme: 'outline', size: 'large', shape: 'rectangular', text: 'signin_with' });
  };
  init();
  if (!state.config.google_client_id) actions.append(el('p', { class: 'muted small' }, 'GOOGLE_CLIENT_ID is not configured on the server.'));
  if (state.config.dev_login) {
    const email = el('input', { type: 'email', id: 'dev-email', placeholder: 'name@example.com' });
    actions.append(el('div', { class: 'dev' }, el('label', { class: 'field', for: 'dev-email' }, 'Email (dev login)'), el('div', { class: 'row' }, email, el('button', {
      onclick: guard(async () => {
        state.me = await api('POST', '/auth/dev', { email: email.value });
        await loadAll();
        render();
        play(document.getElementById('top'), 'drop', 1000);
        enterPage();
      }),
    }, 'Sign in'))));
  }
  return wrap;
}

// ---------- trigger ----------

function triggerView() {
  const v = el('div');
  if (!state.devices.length) {
    v.append(el('div', { class: 'card' }, el('p', {}, 'No PCs are paired yet.'),
      el('p', { class: 'muted small' }, state.me.role === 'admin' ? 'Go to Admin, then PCs, then Add PC.' : 'Ask an admin to pair a PC.')));
    return v;
  }
  const groupSel = (g) => `g:${g.id}`;
  const validSel = state.sel.device === 'all' || state.devices.some((d) => d.name === state.sel.device)
    || state.groups.some((g) => groupSel(g) === state.sel.device && g.device_ids.length);
  if (!validSel) state.sel.device = state.devices[0].name;
  if (state.types.length && !state.types.some((t) => String(t.id) === state.sel.type)) state.sel.type = String(state.types[0].id);

  const pick = (key, val) => { state.sel[key] = val; localStorage.setItem('ag.' + key, val); fx.picked = `${key}:${val}`; render(); };
  const popIf = (key, val) => (fx.picked === `${key}:${val}` ? ' pop' : '');

  const chips = el('div', { class: 'chips' });
  for (const d of state.devices) {
    const via = (d.installs || []).length > 1 ? d.installs.find((i) => i.online) : null;
    chips.append(el('button', { class: 'chip' + popIf('device', d.name), 'aria-pressed': String(state.sel.device === d.name), onclick: () => pick('device', d.name),
      title: via ? `Running ${installName(via)}` : null }, dot(d.online), d.name));
  }
  for (const g of state.groups.filter((g) => g.device_ids.length)) {
    const online = state.devices.filter((d) => g.device_ids.includes(d.id) && d.online).length;
    chips.append(el('button', { class: 'chip' + popIf('device', groupSel(g)), 'aria-pressed': String(state.sel.device === groupSel(g)), onclick: () => pick('device', groupSel(g)),
      title: state.devices.filter((d) => g.device_ids.includes(d.id)).map((d) => d.name).join(', ') },
    dot(online), g.name, el('span', { class: 'count' }, `${online}/${g.device_ids.length}`, el('span', { class: 'sr-only' }, ' online'))));
  }
  if (state.devices.length > 1) {
    chips.append(el('button', { class: 'chip' + popIf('device', 'all'), 'aria-pressed': String(state.sel.device === 'all'), onclick: () => pick('device', 'all') }, 'All PCs'));
  }
  v.append(el('h2', {}, 'PC'), chips);

  if (state.types.length) {
    const grid = el('div', { class: 'types' });
    for (const t of state.types) {
      const on = state.sel.type === String(t.id);
      grid.append(el('button', { class: 'type' + popIf('type', String(t.id)), 'aria-pressed': String(on), onclick: () => pick('type', String(t.id)) },
        el('img', { src: mediaURL(t, 'gif'), alt: '', loading: 'lazy' }), el('span', {}, on ? icon('check') : null, t.name)));
    }
    v.append(el('h2', {}, 'Type'), grid);
  }

  const msg = el('input', { type: 'text', id: 'msg', maxlength: '200', placeholder: 'e.g. dinner is ready', enterkeyhint: 'send', 'aria-describedby': 'msg-hint' });
  const send = el('button', { class: 'primary big' }, 'Get their attention');
  const go = guard(async () => {
    send.disabled = true;
    send.textContent = 'Sending…';
    send.classList.add('sending');
    try {
      const target = state.sel.device.startsWith('g:') ? { group: Number(state.sel.device.slice(2)) } : { device: state.sel.device };
      const res = await api('POST', '/api/alerts', { ...target, type: state.sel.type, message: msg.value });
      // The event stream may have delivered these already; only cue what it hasn't.
      for (const x of res) {
        if (!state.alerts.some((a) => a.id === x.alert_id)) fx.fresh.add(x.alert_id);
        else if (x.merged) fx.changed.add(x.alert_id);
      }
      state.lastAlertIds = res.map((r) => r.alert_id);
      msg.value = '';
      const missed = res.filter((r) => r.missed).map((r) => r.device);
      const queued = res.filter((r) => !r.online && !r.missed).map((r) => r.device);
      toast(missed.length === res.length ? `${missed.join(', ')} ${missed.length > 1 ? 'are' : 'is'} offline, not sent`
        : missed.length ? `Sent, but ${missed.join(', ')} offline`
          : queued.length ? `${queued.join(', ')} offline, will show when it reconnects`
            : res.some((r) => r.merged) ? 'Added to the open alert' : 'Sent');
      state.alerts = await api('GET', '/api/alerts?limit=50');
      render();
      if (missed.length < res.length) alarm(); else nope();
    } finally { send.disabled = false; send.textContent = 'Get their attention'; send.classList.remove('sending'); }
  });
  msg.addEventListener('keydown', (e) => { if (e.key === 'Enter') go(); });
  send.addEventListener('click', go);
  v.append(el('h2', {}, el('label', { for: 'msg' }, 'Message (optional)')), msg, el('div', { class: 'hint', id: 'msg-hint' }, 'Up to 200 characters.'),
    send, liveStatus());
  return v;
}

function liveStatus() {
  const box = el('div', { id: 'live' });
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
  missed: 'PC was offline, not delivered',
};

// Status tag: tone plus a word, never colour alone.
const statusTag = {
  pending: ['info', 'Waiting'],
  delivered: ['live', 'On screen'],
  replied: ['section', 'Replied'],
  dismissed: ['neutral', 'Dismissed'],
  cancelled: ['neutral', 'Cancelled'],
  missed: ['correction', 'Not delivered'],
};

function alertCard(a, withCancel) {
  const online = state.devices.find((d) => d.id === a.device_id)?.online;
  const who = a.requests.map((r) => r.name).join(', ');
  const msgs = a.requests.filter((r) => r.message).map((r) => `${r.name}: “${r.message}”`);
  const [tone, word] = statusTag[a.status] || ['neutral', a.status];
  const cue = fx.fresh.delete(a.id) ? ' is-new' : fx.changed.delete(a.id) ? ' is-changed' : '';
  const c = el('article', { class: `card alert ${a.status}${cue}` },
    el('div', { class: 'alert-meta' }, tag(tone, word),
      el('span', { class: 'dateline' }, [a.device_name, a.type_name, ago(a.created)].filter(Boolean).join(' · '))),
    a.status === 'replied' ? el('p', { class: 'alert-title reply' }, a.reply) : el('p', { class: 'alert-title' },
      a.status === 'pending' && !online ? 'PC offline, will show when it reconnects' : statusText[a.status]),
    msgs.length ? el('p', { class: 'alert-quote' }, msgs.join(' · ')) : null,
    el('div', { class: 'row' },
      el('div', { class: 'grow muted small' }, `From ${who}`, duration(a) ? ` · answered in ${duration(a)}` : ''),
      withCancel && (a.status === 'pending' || a.status === 'delivered')
        ? el('button', { class: 'link', onclick: guard(async () => { await api('POST', `/api/alerts/${a.id}/cancel`); }) }, 'Cancel')
        : null),
  );
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
  const nameInput = el('input', { type: 'text', id: 'display-name', 'aria-describedby': 'display-name-hint', maxlength: '40', value: state.me.display_name, placeholder: firstName(state.me.name || state.me.email) });
  const saveName = guard(async () => {
    state.me = await api('PATCH', '/api/me', { display_name: nameInput.value });
    toast(state.me.display_name ? `You'll show up as ${state.me.display_name}` : 'Using your Google name');
    render();
  });
  nameInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') saveName(); });
  v.append(el('h2', {}, 'Account'), el('div', { class: 'card stack' },
    el('div', { class: 'row' },
      state.me.picture ? el('img', { class: 'avatar', src: state.me.picture, alt: '', referrerpolicy: 'no-referrer' }) : null,
      el('div', { class: 'grow' }, el('div', { class: 'item-name' }, shownName(state.me)), el('div', { class: 'dateline' }, state.me.email)),
      el('button', { onclick: guard(async () => { await api('POST', '/auth/logout'); state.me = null; es?.close(); render(); }) }, 'Sign out')),
    el('div', {}, el('label', { class: 'field', for: 'display-name' }, 'Your name on the PC popup'),
      el('div', { class: 'row' }, el('div', { class: 'grow' }, nameInput), el('button', { onclick: saveName }, 'Save')),
      el('div', { class: 'hint', id: 'display-name-hint' }, 'Leave it empty to use your Google first name.'))));

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
      class: 'chip' + (fx.picked === `pref:${val}` ? ' pop' : ''), 'aria-pressed': String(state.me.notify_pref === val),
      onclick: guard(async () => { state.me = await api('PATCH', '/api/me', { notify_pref: val }); fx.picked = `pref:${val}`; render(); }),
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
    Promise.all([api('GET', '/api/admin/users'), api('GET', '/api/admin/keys'), api('GET', '/api/admin/settings')]).then(([users, keys, settings]) => {
      state.admin = { users, keys, settings };
      render();
    }).catch((e) => toast(e.message));
    v.append(el('p', { class: 'muted' }, 'Loading…'));
    return v;
  }
  const reload = async () => {
    const [users, keys, settings, devices, groups, types, presets] = await Promise.all([
      api('GET', '/api/admin/users'), api('GET', '/api/admin/keys'), api('GET', '/api/admin/settings'),
      api('GET', '/api/devices'), api('GET', '/api/groups'), api('GET', '/api/types'), api('GET', '/api/presets'),
    ]);
    Object.assign(state, { devices, groups, types, presets });
    state.admin = { ...state.admin, users, keys, settings };
    render();
  };
  v.append(pcsSection(reload), groupsSection(reload), deliverySection(reload), typesSection(reload), keysSection(reload),
    usersSection(reload), presetsSection(reload));
  return v;
}

const OS_NAMES = { linux: 'Linux', windows: 'Windows' };
// An install is named by its OS when the client reports it (1.1.0+), else by the name it was paired under.
const installName = (i) => OS_NAMES[i.os] || i.label;

// Client versions: 1.1.0+ report theirs. "Update available" means older than the newest version
// any PC reports, so it needs no lookup of the latest release.
const semver = (v) => v.split('.').map((n) => parseInt(n, 10) || 0);
const olderThan = (a, b) => { const [x, y] = [semver(a), semver(b)]; for (let k = 0; k < 3; k++) if (x[k] !== y[k]) return x[k] < y[k]; return false; };
const newestVersion = () => state.devices.flatMap((d) => d.installs || []).map((i) => i.version).filter(Boolean)
  .reduce((best, v) => (!best || olderThan(best, v) ? v : best), '');
function versionInfo(i, newest) {
  if (!i.version && !i.online && !i.last_seen) return null; // never connected
  const behind = newest && (i.version ? olderThan(i.version, newest) : i.online);
  // Connected without a version means a client from before 1.1.0; offline, it may just not have reconnected yet.
  const v = i.version ? `v${i.version}` : i.online ? 'before v1.1.0' : 'version unknown';
  return [v, behind ? el('span', { class: 'update-note' }, 'update available') : null];
}
// A dateline from strings and nodes, separated by middots.
const dateline = (...parts) => el('div', { class: 'dateline' }, parts.flat().filter(Boolean).flatMap((p, k) => (k ? [' · ', p] : [p])));

function pcsSection(reload) {
  const s = el('div', {}, el('h2', {}, 'PCs'));
  const list = el('div', { class: 'card list' });
  for (const d of state.devices) {
    const item = el('div', { class: 'item' });
    const installs = d.installs || [];
    const via = installs.length > 1 ? installs.find((i) => i.online) : null;
    // A merged PC shows the version on each install's row instead.
    const version = installs.length === 1 ? versionInfo(installs[0], newestVersion()) : null;
    const status = d.online ? (via ? `Online on ${installName(via)}` : 'Online')
      : installs.length ? `Last seen ${ago(d.last_seen)}` : 'No install left: pair it again or remove it';
    // replaceChildren would print null as text; filter the optional pieces out.
    const show = () => item.replaceChildren(...[el('span', { class: 'dot' + (d.online ? ' on' : '') }),
      el('div', { class: 'grow' }, el('div', { class: 'item-name' }, d.name), dateline(status, version)),
      el('button', { class: 'link', onclick: edit }, 'Rename'),
      state.devices.length > 1 ? el('button', { class: 'link', onclick: merge }, 'Merge') : null,
      el('button', { class: 'link danger', onclick: guard(async () => {
        if (!confirm(`Remove ${d.name}? It will need to be paired again.`)) return;
        await api('DELETE', `/api/admin/devices/${d.id}`); await reload();
      }) }, 'Remove'),
      installs.length > 1 ? installList(d, reload) : null].filter(Boolean));
    function edit() {
      const name = el('input', { type: 'text', value: d.name, maxlength: '40' });
      const save = guard(async () => {
        if (name.value.trim() === d.name) return show();
        await api('PATCH', `/api/admin/devices/${d.id}`, { name: name.value });
        toast('Renamed'); await reload();
      });
      name.addEventListener('keydown', (e) => { if (e.key === 'Enter') save(); if (e.key === 'Escape') show(); });
      item.replaceChildren(el('div', { class: 'grow' }, name), el('button', { class: 'primary', onclick: save }, 'Save'), el('button', { onclick: show }, 'Cancel'));
      name.focus();
      name.select();
    }
    function merge() {
      const others = state.devices.filter((o) => o.id !== d.id);
      // Preselect the likeliest partner: the longest shared name prefix ("gaming-win" → "gaming").
      const shared = (o) => { let n = 0; while (n < o.name.length && o.name[n].toLowerCase() === d.name[n]?.toLowerCase()) n++; return n; };
      const guess = others.reduce((best, o) => (shared(o) > shared(best) ? o : best), others[0]);
      const into = el('select', { id: `merge-${d.id}` }, others.map((o) => el('option', { value: o.id, selected: o === guess }, o.name)));
      const go = guard(async () => {
        const target = others.find((o) => o.id === Number(into.value));
        await api('POST', `/api/admin/devices/${d.id}/merge`, { into: target.id });
        toast(`Merged into ${target.name}`);
        state.alerts = await api('GET', '/api/alerts?limit=50');
        await reload();
      });
      item.replaceChildren(el('div', { class: 'stack grow pop-in' },
        el('div', {}, el('label', { class: 'field', for: `merge-${d.id}` }, `Merge ${d.name} into`), into),
        el('div', { class: 'muted small' }, 'For a dual-boot PC paired once from each OS: they become one PC, and whichever OS is running is the one that’s online. ',
          `${d.name}’s history, groups and trigger keys move over. You can split it off again later.`),
        el('div', { class: 'row' }, el('button', { class: 'primary', onclick: go }, 'Merge'), el('button', { onclick: show }, 'Cancel'))));
      into.focus();
    }
    show();
    list.append(item);
  }
  const pair = el('div', { class: 'item' });
  pair.append(el('button', { onclick: guard(async () => {
    const { code } = await api('POST', '/api/admin/pairing');
    pair.replaceChildren(installGuide(code));
  }) }, 'Add PC'));
  list.append(pair);
  s.append(list, el('details', { class: 'card' }, el('summary', {}, 'Install or update the PC app'), installGuide(null)));
  return s;
}

// The OSes of a merged (dual-boot) PC, each with its own pairing.
function installList(d, reload) {
  const box = el('div', { class: 'installs' });
  const newest = newestVersion();
  for (const i of d.installs) {
    const row = el('div', { class: 'item' });
    const name = installName(i);
    const show = () => row.replaceChildren(el('span', { class: 'dot' + (i.online ? ' on' : '') }),
      el('div', { class: 'grow' }, el('div', { class: 'install-name' }, name), dateline(
        i.os && i.label !== d.name ? `paired as ${i.label}` : null, versionInfo(i, newest),
        i.online ? 'Online' : `Last seen ${ago(i.last_seen)}`,
      )),
      el('button', { class: 'link', onclick: split }, 'Split off'),
      el('button', { class: 'link danger', onclick: guard(async () => {
        if (!confirm(`Remove ${name} from ${d.name}? That OS will need to be paired again.`)) return;
        await api('DELETE', `/api/admin/devices/${d.id}/installs/${i.id}`); await reload();
      }) }, 'Remove'));
    function split() {
      const input = el('input', { type: 'text', maxlength: '40', 'aria-label': `New PC name for ${name}`,
        value: i.label !== d.name ? i.label : `${d.name}-${(i.os || 'other')}` });
      const save = guard(async () => {
        await api('POST', `/api/admin/devices/${d.id}/installs/${i.id}/split`, { name: input.value });
        toast(`${input.value.trim()} is its own PC again`); await reload();
      });
      input.addEventListener('keydown', (e) => { if (e.key === 'Enter') save(); if (e.key === 'Escape') show(); });
      row.replaceChildren(el('div', { class: 'grow' }, input), el('button', { class: 'primary', onclick: save }, 'Split'), el('button', { onclick: show }, 'Cancel'));
      input.focus();
      input.select();
    }
    show();
    box.append(row);
  }
  return box;
}

const INSTALL = {
  windows: {
    label: 'Windows',
    shell: 'Open PowerShell (Start → type “PowerShell”) and run:',
    cmd: 'irm https://raw.githubusercontent.com/pakkid/attention-getter-v2/main/client/scripts/install.ps1 | iex',
    note: 'Setup opens in its own window. If SmartScreen warns about the app, choose More info → Run anyway.',
  },
  linux: {
    label: 'Linux',
    shell: 'Open a terminal and run:',
    cmd: 'curl -fsSL https://raw.githubusercontent.com/pakkid/attention-getter-v2/main/client/scripts/install.sh | sh',
    note: 'On GNOME, log out and back in once afterwards so the popup can stay out of your way and the hotkey works.',
  },
};
let installOS = /Linux/.test(navigator.userAgent) && !/Android/.test(navigator.userAgent) ? 'linux' : 'windows';

// Step-by-step install instructions; with a pairing code for a new PC, or without one for updates.
function installGuide(code) {
  const box = el('div', { class: 'stack grow' });
  const draw = () => {
    const os = INSTALL[installOS];
    const step = (n, ...kids) => el('div', { class: 'step' }, el('span', { class: 'num' }, n), el('div', { class: 'grow stack' }, ...kids));
    const copyable = (text, cls = 'mono') => el('div', { class: 'secret row' }, el('div', { class: `grow ${cls}` }, text), copyBtn(text));
    box.replaceChildren(
      el('div', { class: 'chips' }, Object.entries(INSTALL).map(([k, o]) => el('button', {
        class: 'chip', 'aria-pressed': String(installOS === k), onclick: () => { installOS = k; draw(); play(box.querySelector('.chip[aria-pressed=true]'), 'pop', 600); },
      }, o.label))),
      step(1, el('div', {}, os.shell), copyable(os.cmd)),
      code
        ? step(2, el('div', {}, 'When setup asks, enter:'),
          el('div', { class: 'muted small' }, 'Server URL'), copyable(location.origin),
          el('div', { class: 'muted small' }, 'Pairing code (expires in 15 minutes)'), copyable(code, 'code'),
          el('div', { class: 'muted small' }, 'Then give the PC a name, pick the monitor for the popup and the hotkey (F13 by default).'),
          el('div', { class: 'muted small' }, 'Dual-boot PC? Pair each OS with the same name and they show up as one PC.'))
        : step(2, el('div', {}, 'A new PC needs a pairing code: close this and tap ', el('strong', {}, 'Add PC'), '.'),
          el('div', { class: 'muted small' }, 'To update a PC that is already set up, run ', el('code', {}, 'attention-getter update'),
            ' on it (version 1.1.0 or later), or run the command above again. Either way it keeps its pairing and settings.')),
      step(3, el('div', {}, code ? 'The PC shows up in this list as soon as it connects.' : 'The PC reconnects by itself after updating.'),
        el('div', { class: 'muted small' }, os.note)),
    );
  };
  draw();
  return box;
}

function groupsSection(reload) {
  const s = el('div', {}, el('h2', {}, 'PC groups'));
  const list = el('div', { class: 'card list' });
  const names = (ids) => state.devices.filter((d) => ids.includes(d.id)).map((d) => d.name).join(', ') || 'No PCs';
  for (const g of state.groups) {
    list.append(el('div', { class: 'item' },
      el('div', { class: 'grow' }, el('div', { class: 'item-name' }, g.name), el('div', { class: 'dateline' }, names(g.device_ids))),
      el('button', { class: 'link', onclick: () => list.replaceWith(groupForm(g, reload)) }, 'Edit'),
      el('button', { class: 'link danger', onclick: guard(async () => {
        if (!confirm(`Delete group ${g.name}? The PCs stay paired.`)) return;
        await api('DELETE', `/api/admin/groups/${g.id}`); await reload();
      }) }, 'Delete')));
  }
  list.append(el('div', { class: 'item' },
    el('button', { disabled: !state.devices.length, onclick: () => list.replaceWith(groupForm(null, reload)) }, 'Add group'),
    state.groups.length ? null : el('div', { class: 'muted small grow' }, 'Trigger several PCs at once, e.g. “Upstairs”. Groups also work as pc=NAME in trigger URLs.')));
  s.append(list);
  return s;
}

function groupForm(g, reload) {
  const name = el('input', { type: 'text', value: g?.name || '', maxlength: '40', placeholder: 'e.g. Upstairs' });
  const boxes = state.devices.map((d) => ({ d, box: el('input', { type: 'checkbox', checked: !!g?.device_ids.includes(d.id) }) }));
  const save = el('button', { class: 'primary' }, g ? 'Save' : 'Create');
  save.addEventListener('click', guard(async () => {
    const device_ids = boxes.filter((b) => b.box.checked).map((b) => b.d.id);
    await api(g ? 'PUT' : 'POST', g ? `/api/admin/groups/${g.id}` : '/api/admin/groups', { name: name.value, device_ids });
    toast('Saved'); await reload();
  }));
  return el('div', { class: 'card stack pop-in' },
    el('div', {}, el('label', { class: 'field' }, 'Name'), name),
    el('div', {}, el('label', { class: 'field' }, 'PCs in this group'),
      boxes.map(({ d, box }) => el('label', { class: 'check' }, box, d.name))),
    el('div', { class: 'row' }, save, el('button', { onclick: () => render() }, 'Cancel')));
}

function deliverySection(reload) {
  const on = state.admin.settings.deliver_offline;
  const box = el('input', { type: 'checkbox', checked: on });
  box.addEventListener('change', guard(async () => {
    box.disabled = true;
    try {
      state.admin.settings = await api('PUT', '/api/admin/settings', { deliver_offline: box.checked });
      toast(box.checked ? 'Offline PCs will get alerts when they reconnect' : 'Alerts for offline PCs will not be sent');
      await reload();
    } finally { box.disabled = false; }
  }));
  return el('div', {}, el('h2', {}, 'Offline PCs'), el('div', { class: 'card stack' },
    el('label', { class: 'check' }, box, 'Send alerts when an offline PC comes back online'),
    el('div', { class: 'muted small' }, on
      ? 'On: an alert for an offline PC waits and pops up when that PC next connects, even hours later.'
      : 'Off: an alert for an offline PC is not sent. The sender sees “offline, not sent” and it shows in History as not delivered.')));
}

function typesSection(reload) {
  const s = el('div', {}, el('h2', {}, 'Attention types'));
  const list = el('div', { class: 'card list' });
  for (const t of state.types) {
    const audio = el('audio', { src: mediaURL(t, 'sound'), preload: 'none' });
    list.append(el('div', { class: 'item' }, el('img', { src: mediaURL(t, 'gif'), alt: '', loading: 'lazy' }),
      el('div', { class: 'grow item-name' }, t.name, audio),
      el('button', { class: 'link', onclick: (e) => {
        const b = e.currentTarget;
        if (audio.paused) { audio.play(); b.textContent = 'Stop'; audio.onended = () => { b.textContent = 'Play'; }; } else { audio.pause(); audio.currentTime = 0; b.textContent = 'Play'; }
      } }, 'Play'),
      el('button', { class: 'link', onclick: () => list.replaceWith(typeForm(t, reload)) }, 'Edit'),
      el('button', { class: 'link danger', onclick: guard(async () => {
        if (!confirm(`Delete type ${t.name}?`)) return;
        await api('DELETE', `/api/admin/types/${t.id}`); await reload();
      }) }, 'Delete')));
  }
  list.append(el('div', { class: 'item' }, el('button', { onclick: () => list.replaceWith(typeForm(null, reload)) }, 'Add type')));
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
  return el('div', { class: 'card stack pop-in' },
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
      el('div', { class: 'grow' }, el('div', { class: 'item-name' }, k.name), el('div', { class: 'dateline' }, [pins, `used ${ago(k.last_used)}`].filter(Boolean).join(' · '))),
      el('button', { class: 'link danger', onclick: guard(async () => {
        if (!confirm(`Revoke key ${k.name}?`)) return;
        await api('DELETE', `/api/admin/keys/${k.id}`); await reload();
      }) }, 'Revoke')));
  }
  const name = el('input', { type: 'text', id: 'key-name', placeholder: 'e.g. Alexa' });
  const pc = el('select', { id: 'key-pc' }, el('option', { value: '' }, 'Any PC (use ?pc=)'), state.devices.map((d) => el('option', { value: d.id }, d.name)));
  const type = el('select', { id: 'key-type' }, el('option', { value: '' }, 'Any type (use ?type=)'), state.types.map((t) => el('option', { value: t.id }, t.name)));
  const out = el('div');
  const create = el('button', { onclick: guard(async () => {
    const { key } = await api('POST', '/api/admin/keys', {
      name: name.value, pinned_device: pc.value ? Number(pc.value) : null, pinned_type: type.value ? Number(type.value) : null,
    });
    const url = `${location.origin}/api/trigger?key=${key}`;
    state.admin.keys = await api('GET', '/api/admin/keys');
    out.replaceChildren(el('div', { class: 'stack' },
      el('div', { class: 'small' }, 'POST to this URL. It is shown only once, so copy it now. Optional params: &pc=NAME|all &type=NAME &message=TEXT'),
      el('div', { class: 'mono secret' }, url),
      el('div', {}, el('button', { onclick: () => navigator.clipboard.writeText(url).then(() => toast('Copied')) }, 'Copy URL'))));
  }) }, 'Create key');
  const field = (id, label, input) => el('div', {}, el('label', { class: 'field', for: id }, label), input);
  list.append(el('div', { class: 'item' }, el('div', { class: 'stack grow' },
    field('key-name', 'Name shown in the popup', name), field('key-pc', 'PC', pc), field('key-type', 'Type', type), el('div', {}, create), out)));
  s.append(list);
  return s;
}

function usersSection(reload) {
  const s = el('div', {}, el('h2', {}, 'Allowed Google accounts'));
  const list = el('div', { class: 'card list' });
  for (const u of state.admin.users) {
    const self = u.email === state.me.email;
    list.append(el('div', { class: 'item' },
      el('div', { class: 'grow' }, el('div', { class: 'item-name' }, shownName(u)), el('div', { class: 'dateline' }, u.display_name && u.name ? `${u.email} · Google: ${u.name}` : u.email)),
      u.role === 'admin' ? tag('info', 'Admin') : tag('neutral', 'User'),
      self ? null : el('button', { class: 'link', onclick: guard(async () => {
        await api('POST', '/api/admin/users', { email: u.email, role: u.role === 'admin' ? 'user' : 'admin' }); await reload();
      }) }, u.role === 'admin' ? 'Make user' : 'Make admin'),
      self ? null : el('button', { class: 'link danger', onclick: guard(async () => {
        if (!confirm(`Remove ${u.email}?`)) return;
        await api('DELETE', `/api/admin/users/${encodeURIComponent(u.email)}`); await reload();
      }) }, 'Remove')));
  }
  const email = el('input', { type: 'email', id: 'add-user', placeholder: 'name@gmail.com' });
  list.append(el('div', { class: 'item' }, el('div', { class: 'grow' }, el('label', { class: 'field', for: 'add-user' }, 'Add a Google account'), email), el('button', { onclick: guard(async () => {
    await api('POST', '/api/admin/users', { email: email.value, role: 'user' }); await reload();
  }) }, 'Add')));
  s.append(list);
  return s;
}

function presetsSection(reload) {
  const ta = el('textarea', { placeholder: 'One per line (max 9)', 'aria-label': 'Quick replies' });
  ta.value = state.presets.join('\n');
  return el('div', {}, el('h2', {}, 'Quick replies (keys 1–9 in the popup)'), el('div', { class: 'card stack' }, ta,
    el('div', {}, el('button', { onclick: guard(async () => {
      await api('PUT', '/api/admin/presets', ta.value.split('\n'));
      toast('Saved'); await reload();
    }) }, 'Save'))));
}

boot().catch((e) => {
  document.getElementById('app').replaceChildren(el('p', { class: 'center muted' }, `Failed to load: ${e.message}`));
});
