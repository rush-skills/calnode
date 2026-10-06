/*
 * Calnode live-status widget: <calnode-live data-base="https://cal.example.com"
 *   data-kind="office_hours" data-poll="30"></calnode-live>
 *
 * A standalone, framework-free Web Component (Shadow DOM, own minimal CSS, no build step)
 * served at GET /live-widget.js. It polls GET {base}/v1/live/status every data-poll seconds
 * (default 30) and renders "Live now · Join" or "Offline · next session at …" inline on
 * any site. data-base defaults to the origin this script was loaded from.
 *
 * Deliberately independent of embed.js and booking-logic.js: it shares no state or
 * helpers with the booking widget, so neither can break the other.
 */
(function () {
  'use strict';
  if (typeof window === 'undefined' || !('customElements' in window)) return;
  if (customElements.get('calnode-live')) return;

  var scriptOrigin = (function () {
    try {
      var src = document.currentScript && document.currentScript.src;
      if (src) return new URL(src, location.href).origin;
    } catch (e) { /* fall through */ }
    return location.origin;
  })();

  var CSS = [
    ':host { display: block; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", sans-serif; color: #111827; font-size: 14px; line-height: 1.5; }',
    '*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }',
    '.box { border: 1px solid #e5e7eb; border-radius: 12px; padding: 14px 16px; background: #fff; display: flex; flex-direction: column; gap: 8px; }',
    '.badge { display: inline-flex; align-items: center; gap: 6px; font-size: 11px; font-weight: 700; letter-spacing: .06em; text-transform: uppercase; padding: 3px 9px; border-radius: 999px; width: fit-content; }',
    '.badge.live { background: #fef2f2; color: #dc2626; }',
    '.badge.live::before { content: ""; width: 7px; height: 7px; border-radius: 50%; background: #dc2626; animation: calnode-pulse 1.4s ease-in-out infinite; }',
    '.badge.off { background: #f3f4f6; color: #6b7280; }',
    '.badge.off::before { content: ""; width: 7px; height: 7px; border-radius: 50%; background: #9ca3af; }',
    '@keyframes calnode-pulse { 0%, 100% { opacity: 1; } 50% { opacity: .35; } }',
    '@media (prefers-reduced-motion: reduce) { .badge.live::before { animation: none; } }',
    '.title { font-weight: 600; font-size: 15px; }',
    '.muted { color: #6b7280; font-size: 13px; }',
    '.join { display: inline-flex; align-items: center; justify-content: center; background: #111827; color: #fff; text-decoration: none; font-weight: 600; font-size: 14px; padding: 8px 14px; border-radius: 8px; width: fit-content; }',
    '.join:hover { opacity: .9; }',
    '.session + .session { border-top: 1px solid #e5e7eb; padding-top: 8px; }'
  ].join('\n');

  // Consecutive failed polls before the widget stops offering Join and says so.
  var STALE_AFTER = 3;

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  // Viewer-local time via Intl; the API sends RFC3339 UTC only.
  function fmtWhen(iso) {
    var d = new Date(iso);
    if (isNaN(d)) return iso;
    var now = new Date();
    var tomorrow = new Date(now); tomorrow.setDate(now.getDate() + 1);
    var time = d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
    if (d.toDateString() === now.toDateString()) return 'today at ' + time;
    if (d.toDateString() === tomorrow.toDateString()) return 'tomorrow at ' + time;
    return d.toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric' }) + ' at ' + time;
  }

  class CalnodeLive extends HTMLElement {
    constructor() {
      super();
      this._timer = null;
      this._rendered = false;
    }

    connectedCallback() {
      if (!this.shadowRoot) {
        var root = this.attachShadow({ mode: 'open' });
        var style = document.createElement('style');
        style.textContent = CSS;
        root.appendChild(style);
        this._box = el('div', 'box');
        this._box.appendChild(el('span', 'muted', 'Checking who\u2019s live\u2026'));
        root.appendChild(this._box);
      }
      this._base = (this.getAttribute('data-base') || scriptOrigin).replace(/\/+$/, '');
      this._kind = this.getAttribute('data-kind') || '';
      var poll = parseInt(this.getAttribute('data-poll'), 10);
      this._pollMs = (poll > 0 ? Math.max(poll, 5) : 30) * 1000;
      var that = this;
      this._onVisible = function () { if (!document.hidden) that.poll(); };
      document.addEventListener('visibilitychange', this._onVisible);
      this.poll();
      this._timer = setInterval(function () { that.poll(); }, this._pollMs);
    }

    disconnectedCallback() {
      if (this._timer) clearInterval(this._timer);
      this._timer = null;
      document.removeEventListener('visibilitychange', this._onVisible);
    }

    poll() {
      var that = this;
      var url = this._base + '/v1/live/status' + (this._kind ? '?kind=' + encodeURIComponent(this._kind) : '');
      fetch(url, { cache: 'no-store', mode: 'cors' })
        .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
        .then(function (data) { that._failures = 0; that._last = data; that.render(data); })
        .catch(function () {
          if (!that._rendered) {
            that._box.textContent = '';
            that._box.appendChild(el('span', 'muted', 'Live status unavailable.'));
            return;
          }
          // After STALE_AFTER consecutive failed polls the last good status is still shown,
          // but without the Join button (the session may have ended) and with a notice.
          // The next successful poll renders normally again.
          that._failures = (that._failures || 0) + 1;
          if (that._failures === STALE_AFTER && that._last) that.render(that._last, true);
        });
    }

    render(data, stale) {
      var box = this._box;
      box.textContent = '';
      var live = data.live || [];
      if (live.length) {
        live.forEach(function (s) {
          var wrap = el('div', 'session');
          wrap.appendChild(el('span', 'badge live', 'Live now'));
          wrap.appendChild(el('div', 'title', s.title));
          if (s.host_name) wrap.appendChild(el('div', 'muted', 'Hosted by ' + s.host_name));
          if (s.join_url && !stale) {
            var a = el('a', 'join', 'Join now');
            a.href = s.join_url; a.target = '_blank'; a.rel = 'noopener noreferrer';
            wrap.appendChild(a);
          }
          box.appendChild(wrap);
        });
      } else {
        box.appendChild(el('span', 'badge off', 'Offline'));
        var next = data.next;
        if (next) {
          box.appendChild(el('div', 'title', next.title));
          box.appendChild(el('div', 'muted', 'Next session ' + fmtWhen(next.scheduled_start_at) + (next.host_name ? ' \u00b7 ' + next.host_name : '')));
        } else {
          box.appendChild(el('div', 'muted', 'No session scheduled yet.'));
        }
      }
      if (stale) box.appendChild(el('div', 'muted', 'Live status may be out of date — we could not reach the server.'));
      this._rendered = true;
      this.dispatchEvent(new CustomEvent('calnode-live:update', { detail: data }));
    }
  }

  customElements.define('calnode-live', CalnodeLive);
})();
