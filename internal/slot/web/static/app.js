// Links and forms load over fetch and swap the page body in place, so the app
// navigates without full reloads. The server still renders every page, and the
// HTML works on its own when this script does not run.
const parser = new DOMParser();
const progress = document.createElement('div');
progress.className = 'progress';
const toasts = document.createElement('div');
toasts.className = 'toasts';
toasts.setAttribute('aria-live', 'polite');
const prefetches = new Map();
let navigation = null;
let pendingTimer = 0;
let hoverTimer = 0;
// The page on screen; hash-only history steps within it need no fetch.
let shown = location.pathname + location.search;

history.scrollRestoration = 'manual';
document.body.append(progress, toasts);

function toast(message, error = false) {
  const item = document.createElement('div');
  item.className = error ? 'alert error' : 'alert';
  item.setAttribute('role', error ? 'alert' : 'status');
  item.textContent = message;
  dismissible(item);
  toasts.append(item);
  while (toasts.children.length > 3) toasts.firstChild.remove();
  window.setTimeout(() => item.remove(), error ? 10000 : 5000);
}

function dismissible(alert) {
  const close = document.createElement('button');
  close.type = 'button';
  close.className = 'alert-close';
  close.setAttribute('aria-label', 'Dismiss');
  close.textContent = '×';
  close.addEventListener('click', () => alert.remove());
  alert.append(close);
}

function confirmed(message, label) {
  const dialog = document.createElement('dialog');
  dialog.className = 'confirm';
  const text = document.createElement('p');
  text.textContent = message;
  const actions = document.createElement('form');
  actions.method = 'dialog';
  actions.className = 'confirm-actions';
  const keep = document.createElement('button');
  keep.className = 'secondary';
  keep.value = 'keep';
  keep.textContent = 'Never mind';
  const proceed = document.createElement('button');
  proceed.className = 'danger-button';
  proceed.value = 'ok';
  proceed.textContent = label;
  actions.append(keep, proceed);
  dialog.append(text, actions);
  document.body.append(dialog);
  dialog.showModal();
  keep.focus();
  return new Promise(resolve => dialog.addEventListener('close', () => {
    dialog.remove();
    resolve(dialog.returnValue === 'ok');
  }));
}

function boostable(url) {
  return url.origin === location.origin && !url.pathname.startsWith('/static/');
}

function samePage(url) {
  return url.pathname + url.search === shown;
}

function linkBoostable(link) {
  const url = new URL(link.href);
  if (link.target || link.hasAttribute('download') || link.dataset.boost === 'false') return false;
  // In-page anchors keep the browser's own scrolling.
  if (samePage(url) && url.hash) return false;
  return boostable(url);
}

// Prefetch on hover so a click usually finds the page already loaded.
function prefetch(link) {
  const url = new URL(link.href);
  url.hash = '';
  if (!linkBoostable(link) || samePage(url) || prefetches.has(url.href)) return;
  prefetches.set(url.href, fetch(url, { headers: { Accept: 'text/html' } }).catch(() => null));
  window.setTimeout(() => prefetches.delete(url.href), 10000);
}

async function navigate(href, { method = 'GET', body = null, mode = 'push', scroll } = {}) {
  navigation?.abort();
  const current = navigation = new AbortController();
  const url = new URL(href, location.href);
  const busy = window.setTimeout(() => progress.classList.add('active'), 120);
  try {
    const key = new URL(url);
    key.hash = '';
    let response = method === 'GET' ? await prefetches.get(key.href) : null;
    prefetches.delete(key.href);
    response ??= await fetch(url, { method, body, signal: current.signal, headers: { Accept: 'text/html' } });
    const type = response.headers.get('Content-Type') || '';
    if (!type.startsWith('text/html')) {
      // Downloads and plain-text error pages are the browser's to show.
      if (method === 'GET') {
        location.assign(url);
        return;
      }
      toast((await response.text()).trim() || 'Something went wrong. Please try again.', true);
      return;
    }
    const page = parser.parseFromString(await response.text(), 'text/html');
    if (current.signal.aborted) return;
    if (method !== 'GET' && !response.ok) {
      // Keep the form and what was typed into it; just say what went wrong.
      toast(page.querySelector('[role=alert]')?.textContent.trim() || 'Something went wrong. Please try again.', true);
      return;
    }
    const target = new URL(response.url);
    // Redirects drop the fragment, so carry over the one that was asked for.
    if (!target.hash) target.hash = url.hash;
    render(page, target, mode, scroll);
  } catch (error) {
    if (error.name === 'AbortError') return;
    if (method === 'GET') {
      location.assign(url);
      return;
    }
    toast('Could not reach the server. Check your connection and try again.', true);
  } finally {
    window.clearTimeout(busy);
    if (navigation === current) {
      progress.classList.remove('active');
      navigation = null;
    }
  }
}

function render(page, url, mode, scroll) {
  const stay = samePage(url) || url.pathname === location.pathname;
  const swap = () => {
    if (mode === 'push' && url.href !== location.href) {
      history.replaceState({ scroll: window.scrollY }, '');
      history.pushState({ scroll: 0 }, '', url);
    } else if (mode === 'replace') {
      history.replaceState({ scroll: window.scrollY }, '', url);
    }
    shown = url.pathname + url.search;
    document.title = page.title;
    const icon = page.querySelector('link[rel=icon]');
    if (icon) document.querySelector('link[rel=icon]').setAttribute('href', icon.getAttribute('href'));
    document.body.replaceChildren(...page.body.childNodes, progress, toasts);
    enhance();
    if (scroll !== undefined) {
      window.scrollTo({ top: scroll, behavior: 'instant' });
    } else if (url.hash) {
      document.getElementById(decodeURIComponent(url.hash.slice(1)))?.scrollIntoView();
    } else if (!stay) {
      window.scrollTo({ top: 0, behavior: 'instant' });
    }
    if (!stay) {
      const main = document.querySelector('main');
      main?.setAttribute('tabindex', '-1');
      main?.focus({ preventScroll: true });
    }
  };
  if (document.startViewTransition && !document.hidden) document.startViewTransition(swap);
  else swap();
}

// Runs on every page, first load or swapped in.
function enhance() {
  document.querySelectorAll('input[data-detect-timezone]').forEach(input => {
    if (!input.value) input.value = Intl.DateTimeFormat().resolvedOptions().timeZone || '';
  });
  const guestPage = document.querySelector('[data-detect-guest-timezone]');
  const guestZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  if (guestPage && guestZone && guestZone !== guestPage.dataset.detectGuestTimezone) {
    const url = new URL(location.href);
    url.searchParams.set('tz', guestZone);
    navigate(url.href, { mode: 'replace', scroll: window.scrollY });
  }
  const slugSource = document.querySelector('[data-slug-source]');
  const slugTarget = document.querySelector('[data-slug-target]');
  if (slugSource && slugTarget) {
    const preview = document.querySelector('[data-slug-preview]');
    // Suggest only: a blank field lets the server pick a unique slug.
    const suggest = () => {
      slugTarget.placeholder = slugSource.value.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+/, '').slice(0, 40).replace(/-+$/, '') || 'meeting';
      preview.textContent = slugTarget.value || slugTarget.placeholder;
    };
    slugSource.addEventListener('input', suggest);
    slugTarget.addEventListener('input', suggest);
  }
  // Confirmations float over the page, so they show even when the change was
  // made far below the top.
  document.querySelectorAll('main .alert[role=status]').forEach(alert => {
    toast(alert.textContent.trim());
    alert.remove();
  });
  document.querySelectorAll('main .alert[role=alert]').forEach(dismissible);
  // Drop ?notice= so a reload does not show the same confirmation again.
  const url = new URL(location.href);
  if (url.searchParams.has('notice')) {
    url.searchParams.delete('notice');
    history.replaceState(history.state, '', url);
    shown = url.pathname + url.search;
  }
  window.clearTimeout(pendingTimer);
  if (document.querySelector('[data-pending]')) pendingTimer = window.setTimeout(refreshPending, 3000);
}

function refreshPending() {
  if (!document.querySelector('[data-pending]')) return;
  if (document.hidden) {
    document.addEventListener('visibilitychange', refreshPending, { once: true });
    return;
  }
  navigate(location.href, { mode: 'none', scroll: window.scrollY });
}

document.addEventListener('click', event => {
  const link = event.target.closest('a[href]');
  if (!link || event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  if (!linkBoostable(link)) return;
  event.preventDefault();
  navigate(link.href);
});

document.addEventListener('mouseover', event => {
  const link = event.target.closest('a[href]');
  window.clearTimeout(hoverTimer);
  if (link) hoverTimer = window.setTimeout(() => prefetch(link), 80);
});

document.addEventListener('touchstart', event => {
  const link = event.target.closest('a[href]');
  if (link) prefetch(link);
}, { passive: true });

document.addEventListener('submit', async event => {
  const form = event.target;
  if (event.defaultPrevented || form.method === 'dialog' || form.dataset.boost === 'false') return;
  const action = new URL(form.action);
  if (!boostable(action)) return;
  event.preventDefault();
  if (form.getAttribute('aria-busy') === 'true') return;
  const submitter = event.submitter;
  if (form.dataset.confirm && !await confirmed(form.dataset.confirm, submitter?.textContent.trim() || 'Continue')) return;
  // URL-encoded rather than FormData's multipart, which the server does not parse.
  const data = new URLSearchParams(new FormData(form, submitter));
  const buttons = form.querySelectorAll('button');
  form.setAttribute('aria-busy', 'true');
  buttons.forEach(button => { button.disabled = true; });
  try {
    if (form.method === 'get') {
      action.search = data;
      await navigate(action.href);
    } else {
      await navigate(action.href, { method: 'POST', body: data });
    }
  } finally {
    form.removeAttribute('aria-busy');
    buttons.forEach(button => { button.disabled = false; });
  }
});

window.addEventListener('popstate', event => {
  if (location.pathname + location.search === shown) return;
  navigate(location.href, { mode: 'none', scroll: event.state?.scroll ?? 0 });
});

history.replaceState({ scroll: window.scrollY }, '');
enhance();
