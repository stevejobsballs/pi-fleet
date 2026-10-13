// Quick phrases (Work done on a work order): a tap adds the phrase to the
// text box, on a line of its own, where it can be changed. Without this
// script the phrases stay hidden and the box works as before.
"use strict";

for (const box of document.querySelectorAll(".phrases[data-for]")) {
  const text = document.getElementById(box.dataset.for);
  if (!text) continue;
  box.hidden = false;
  box.addEventListener("click", (e) => {
    const b = e.target.closest("button.phrase");
    if (!b) return;
    const v = text.value.replace(/\s+$/, "");
    text.value = (v ? v + "\n" : "") + b.dataset.phrase;
    text.focus();
    text.setSelectionRange(text.value.length, text.value.length);
    b.classList.add("used");
  });
}
