// tz-picker.js — the searchable timezone picker on book.html and manage.html. Inlined after
// booking-logic.js (book.go appends it), so BookingLogic is in scope. Vanilla DOM, no build.
// The embed widget has its own in-shadow-root version (embed.js); keep the behaviour aligned.
//
// mountTzPicker(root, opts): root is an element holding the trigger. opts:
//   value   current IANA zone
//   labels  { change, search, empty, aria } (translated strings from the page)
//   onChange(zone) called after the visitor picks a different zone
// Returns { value(), set(zone) }.
(function (root) {
  'use strict';
  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }
  root.mountTzPicker = function (host, opts) {
    var B = root.BookingLogic;
    var raw = [];
    try { if (Intl.supportedValuesOf) raw = Intl.supportedValuesOf('timeZone'); } catch (e) { raw = []; }
    var current = B.canonicalTz(opts.value);
    var list = null;           // built on first open: ~420 Intl calls each
    var labels = opts.labels || {};
    var uid = 'tzp' + Math.random().toString(36).slice(2, 8);

    var btn = el('button', 'tzp-btn');
    btn.type = 'button';
    btn.setAttribute('aria-haspopup', 'listbox');
    btn.setAttribute('aria-expanded', 'false');
    btn.title = labels.change || '';
    var btnText = el('span', 'tzp-btn-text');
    btn.appendChild(btnText);
    btn.insertAdjacentHTML('beforeend', '<svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="6 9 12 15 18 9"/></svg>');

    var pop = el('div', 'tzp-pop');
    pop.hidden = true;
    var input = el('input', 'tzp-search');
    input.type = 'text';
    input.placeholder = labels.search || '';
    input.setAttribute('aria-label', labels.search || '');
    input.setAttribute('autocomplete', 'off');
    input.setAttribute('spellcheck', 'false');
    input.setAttribute('role', 'combobox');
    input.setAttribute('aria-expanded', 'true');
    input.setAttribute('aria-controls', uid + '-list');
    input.setAttribute('aria-autocomplete', 'list');
    var ul = el('ul', 'tzp-list');
    ul.id = uid + '-list';
    ul.setAttribute('role', 'listbox');
    ul.setAttribute('aria-label', labels.aria || '');
    pop.appendChild(input);
    pop.appendChild(ul);
    host.appendChild(btn);
    host.appendChild(pop);

    var shown = [], active = -1;
    function zoneLabel(id) { return id.replace(/_/g, ' '); }
    function gmtOf(id) {
      var z = (list || []).filter(function (x) { return x.id === id; })[0];
      if (z) return z.gmt;
      try {
        var p = new Intl.DateTimeFormat('en-US', { timeZone: id, timeZoneName: 'shortOffset' }).formatToParts(new Date());
        for (var i = 0; i < p.length; i++) if (p[i].type === 'timeZoneName') return p[i].value;
      } catch (e) { /* ignore */ }
      return '';
    }
    function paintButton() {
      var g = gmtOf(current);
      btnText.textContent = zoneLabel(current) + (g ? ' (' + g + ')' : '');
    }
    function setActive(i) {
      var items = ul.children;
      if (active >= 0 && items[active]) items[active].classList.remove('active');
      active = i;
      if (i >= 0 && items[i]) {
        items[i].classList.add('active');
        input.setAttribute('aria-activedescendant', items[i].id);
        // Scroll the list only, never the page.
        var it = items[i], top = it.offsetTop, bot = top + it.offsetHeight;
        if (top < ul.scrollTop) ul.scrollTop = top;
        else if (bot > ul.scrollTop + ul.clientHeight) ul.scrollTop = bot - ul.clientHeight;
      } else input.removeAttribute('aria-activedescendant');
    }
    function render() {
      shown = B.tzMatches(list, input.value);
      ul.textContent = '';
      if (!shown.length) {
        var empty = el('li', 'tzp-empty', labels.empty || '');
        empty.setAttribute('role', 'presentation');
        ul.appendChild(empty);
        setActive(-1);
        return;
      }
      shown.forEach(function (z, i) {
        var li = el('li', 'tzp-opt');
        li.id = uid + '-o' + i;
        li.setAttribute('role', 'option');
        li.setAttribute('aria-selected', String(z.id === current));
        li.appendChild(el('span', 'tzp-name', zoneLabel(z.id)));
        li.appendChild(el('span', 'tzp-gmt', z.gmt));
        li.addEventListener('mousedown', function (e) { e.preventDefault(); });
        li.addEventListener('click', function () { choose(z.id); });
        ul.appendChild(li);
      });
      var cur = input.value ? 0 : shown.map(function (z) { return z.id; }).indexOf(current);
      setActive(cur < 0 ? 0 : cur);
      if (!input.value && cur > 0) {
        var it = ul.children[cur];
        ul.scrollTop = Math.max(0, it.offsetTop - ul.clientHeight / 2 + it.offsetHeight / 2);
      }
    }
    function open() {
      if (!list) list = B.tzList(raw, current);
      pop.hidden = false;
      btn.setAttribute('aria-expanded', 'true');
      host.classList.add('tzp-open');
      input.value = '';
      render();
      input.focus({ preventScroll: true });
    }
    function close(refocus) {
      if (pop.hidden) return;
      pop.hidden = true;
      btn.setAttribute('aria-expanded', 'false');
      host.classList.remove('tzp-open');
      if (refocus) btn.focus({ preventScroll: true });
    }
    function choose(id) {
      close(true);
      if (id === current) return;
      current = id;
      paintButton();
      if (opts.onChange) opts.onChange(id);
    }
    btn.addEventListener('click', function () { pop.hidden ? open() : close(true); });
    input.addEventListener('input', render);
    input.addEventListener('keydown', function (e) {
      var n = shown.length;
      if (e.key === 'ArrowDown') { e.preventDefault(); if (n) setActive((active + 1) % n); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); if (n) setActive((active - 1 + n) % n); }
      else if (e.key === 'Enter') { e.preventDefault(); if (active >= 0 && shown[active]) choose(shown[active].id); }
      else if (e.key === 'Escape') { e.preventDefault(); close(true); }
      else if (e.key === 'Tab') { close(false); }
    });
    document.addEventListener('mousedown', function (e) { if (!host.contains(e.target)) close(false); });
    paintButton();
    return {
      value: function () { return current; },
      set: function (zone) { current = B.canonicalTz(zone); paintButton(); }
    };
  };
})(typeof self !== 'undefined' ? self : this);
