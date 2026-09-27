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
