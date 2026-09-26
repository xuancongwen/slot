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
