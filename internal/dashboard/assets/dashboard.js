// Conflux dashboard — tiny HTMX helper. No framework.
// Responsibilities: show a toast on HTMX action outcomes, guard destructive
// actions with a confirm, manage 2s Live Mode auto-refresh, and keep the
// clock/relative-times fresh.
(function () {
  "use strict";

  const LIVE_INTERVAL_MS = 2000;
  const LIVE_STORAGE_KEY = "conflux_live_mode";

  function getLiveMode() {
    const saved = localStorage.getItem(LIVE_STORAGE_KEY);
    return saved === null || saved === "true";
  }

  function setLiveMode(enabled) {
    localStorage.setItem(LIVE_STORAGE_KEY, enabled ? "true" : "false");
    updateLiveUI(enabled);
    toast(enabled ? "Live mode enabled (2s interval)" : "Live mode paused", enabled ? "ok" : "warn");
    if (enabled) {
      triggerLiveTick();
    }
  }

  function updateLiveUI(enabled) {
    const toggle = document.getElementById("live-mode-toggle");
    if (toggle) toggle.checked = enabled;
    const dot = document.getElementById("live-indicator");
    if (dot) {
      if (enabled) {
        dot.classList.remove("paused");
      } else {
        dot.classList.add("paused");
      }
    }
    const container = document.getElementById("live-toggle-btn");
    if (container) {
      container.setAttribute("aria-checked", enabled ? "true" : "false");
    }
  }

  function triggerLiveTick() {
    if (document.hidden) return;
    document.body.dispatchEvent(new CustomEvent("liveTick"));
    refreshTimes();
  }

  // Live Mode 2s tick interval
  setInterval(function () {
    if (getLiveMode()) {
      triggerLiveTick();
    }
  }, LIVE_INTERVAL_MS);

  // Initialize live toggle on DOM load and after swaps
  function initLiveToggle() {
    const toggle = document.getElementById("live-mode-toggle");
    const container = document.getElementById("live-toggle-btn");
    const isLive = getLiveMode();
    updateLiveUI(isLive);

    if (toggle && !toggle._bound) {
      toggle._bound = true;
      toggle.addEventListener("change", function (e) {
        setLiveMode(e.target.checked);
      });
    }

    if (container && !container._bound) {
      container._bound = true;
      container.addEventListener("click", function (e) {
        if (e.target.tagName.toLowerCase() === "input") return;
        const next = !getLiveMode();
        setLiveMode(next);
      });
      container.addEventListener("keydown", function (e) {
        if (e.key === " " || e.key === "Enter") {
          e.preventDefault();
          const next = !getLiveMode();
          setLiveMode(next);
        }
      });
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", initLiveToggle);
  } else {
    initLiveToggle();
  }

  // Toast feedback after an HTMX swap triggered by a [data-toast] button.
  document.body.addEventListener("htmx:afterRequest", function (e) {
    const elt = e.detail.requestConfig.elt;
    if (!elt || !elt.hasAttribute("data-toast")) return;
    const ok = e.detail.successful;
    const msg = elt.getAttribute("data-toast") || (ok ? "done" : "request failed");
    toast(msg, ok ? "ok" : "err");
  });

  // Confirm destructive actions before HTMX sends the request.
  document.body.addEventListener("htmx:confirm", function (e) {
    const msg = e.target.getAttribute("data-confirm");
    if (!msg) return;
    e.preventDefault();
    if (window.confirm(msg)) {
      e.detail.issueRequest(e);
    }
  });

  // A failed reload should surface the error text from the response body.
  document.body.addEventListener("htmx:responseError", function (e) {
    const elt = e.detail.requestConfig.elt;
    if (!elt || !elt.hasAttribute("data-toast")) return;
    let msg = elt.getAttribute("data-toast") || "request failed";
    try {
      const body = e.detail.xhr.responseText;
      if (body && body.length < 400) msg = body;
    } catch (_) {}
    toast(msg, "err");
  });

  function toast(msg, kind) {
    let el = document.getElementById("toast");
    if (!el) {
      el = document.createElement("div");
      el.id = "toast";
      el.className = "toast";
      document.body.appendChild(el);
    }
    el.textContent = msg;
    el.className = "toast " + (kind || "");
    // force reflow so the transition replays
    void el.offsetWidth;
    el.classList.add("show");
    clearTimeout(el._t);
    el._t = setTimeout(function () {
      el.classList.remove("show");
    }, 3200);
  }

  // Refresh relative times for elements carrying a data-epoch (ms).
  function refreshTimes() {
    document.querySelectorAll("[data-epoch]").forEach(function (n) {
      const ms = parseInt(n.getAttribute("data-epoch"), 10);
      if (!ms) return;
      n.textContent = rel(ms);
    });
  }

  function rel(ms) {
    const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
    if (s < 60) return s + "s ago";
    if (s < 3600) return Math.round(s / 60) + "m ago";
    if (s < 86400) return Math.round(s / 3600) + "h ago";
    return Math.round(s / 86400) + "d ago";
  }
})();
