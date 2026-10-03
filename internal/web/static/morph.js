"use strict";

// DOM morphing: brings an element's content up to date with new HTML by
// changing only what differs, instead of replacing it through innerHTML.
// Elements that stay keep their identity, so hover, focus, open drop-downs,
// text selections and clicks in progress survive an update.
//
// Children are matched by position. An element only takes the place of an
// element with the same tag and the same id, data-card and data-key; anything
// else is replaced.

function morph(target, html) {
  const tpl = document.createElement("template");
  tpl.innerHTML = html;
  morphChildren(target, tpl.content);
}

function morphSame(a, b) {
  if (a.nodeType !== b.nodeType || a.nodeName !== b.nodeName) return false;
  if (a.nodeType !== Node.ELEMENT_NODE) return true;
  return ["id", "data-card", "data-key"].every((n) => a.getAttribute(n) === b.getAttribute(n));
}

function morphChildren(to, from) {
  const want = [...from.childNodes];
  const have = [...to.childNodes];
  want.forEach((w, i) => {
    const h = have[i];
    if (!h) to.appendChild(w);
    else if (morphSame(h, w)) morphNode(h, w);
    else to.replaceChild(w, h);
  });
  for (let i = want.length; i < have.length; i++) have[i].remove();
}

function morphNode(to, from) {
  if (to.nodeType !== Node.ELEMENT_NODE) {
    if (to.nodeValue !== from.nodeValue) to.nodeValue = from.nodeValue;
    return;
  }
  for (const { name, value } of [...from.attributes]) {
    if (to.getAttribute(name) !== value) to.setAttribute(name, value);
  }
  for (const { name } of [...to.attributes]) {
    if (!from.hasAttribute(name)) to.removeAttribute(name);
  }
  const focused = to === document.activeElement;
  if (to.tagName === "INPUT") {
    // The attribute is only the initial value: the shown value is a property.
    // A focused field keeps what the user is typing.
    if (to.type === "checkbox") to.checked = from.hasAttribute("checked");
    else if (!focused && to.value !== (from.getAttribute("value") ?? "")) to.value = from.getAttribute("value") ?? "";
    return;
  }
  if (to.tagName === "TEXTAREA") return;
  morphChildren(to, from);
  if (to.tagName === "SELECT" && !focused) {
    const sel = [...to.options].find((o) => o.hasAttribute("selected"));
    if (sel && to.value !== sel.value) to.value = sel.value;
  }
}

// Sets an element's text only when it changed.
function setText(el, text) {
  if (el.textContent !== text) el.textContent = text;
}
