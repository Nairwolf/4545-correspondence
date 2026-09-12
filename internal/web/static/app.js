// Submit the standings filter form as soon as the active/inactive select
// changes, so it feels live without a separate "Apply" click. Everything
// still works with JS disabled — the button and plain links remain.
document.addEventListener("change", function (e) {
  if (e.target instanceof HTMLSelectElement && e.target.name === "active") {
    e.target.form && e.target.form.requestSubmit();
  }
});
