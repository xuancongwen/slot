// Progressive enhancement only: all forms and booking flows work without JavaScript.
document.querySelectorAll('form[data-confirm]').forEach(form => {
  form.addEventListener('submit', event => {
    if (!window.confirm(form.dataset.confirm)) event.preventDefault();
  });
});
if (document.querySelector('[data-pending]')) {
  window.setTimeout(() => { if (!document.hidden) window.location.reload(); }, 5000);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) window.location.reload(); });
}
document.querySelectorAll('input[data-detect-timezone]').forEach(input => {
  if (!input.value) input.value = Intl.DateTimeFormat().resolvedOptions().timeZone || '';
});
const guestPage = document.querySelector('[data-detect-guest-timezone]');
const guestZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
if (guestPage && guestZone && guestZone !== guestPage.dataset.detectGuestTimezone) {
  const url = new URL(window.location.href);
  url.searchParams.set('tz', guestZone);
  window.location.replace(url);
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
document.querySelectorAll('.alert[role]').forEach(alert => {
  const close = document.createElement('button');
  close.type = 'button';
  close.className = 'alert-close';
  close.setAttribute('aria-label', 'Dismiss');
  close.textContent = '×';
  close.addEventListener('click', () => alert.remove());
  alert.append(close);
  if (alert.getAttribute('role') === 'status') window.setTimeout(() => alert.remove(), 10000);
});
// Drop ?notice= so a reload does not show the same confirmation again.
const page = new URL(window.location.href);
if (page.searchParams.has('notice')) {
  page.searchParams.delete('notice');
  window.history.replaceState(null, '', page);
}
