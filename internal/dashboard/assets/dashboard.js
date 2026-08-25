// Conflux dashboard — tiny HTMX helper. No framework.
// Responsibilities: show a toast on HTMX action outcomes, guard destructive
// actions with a confirm, and keep the clock/relative-times fresh.
(function () {
  "use strict";

  // Toast feedback after an HTMX swap triggered by a [data-toast] button.
  document.body.addEventListener("htmx:afterRequest", function (e) {
    const elt = e.detail.requestConfig.elt;
    if (!elt || !elt.hasAttribute("data-toast")) return;
    const ok = e.detail.successful;
    const msg = elt.getAttribute("data-toast") || (ok ? "done" : "request failed");
    toast(msg, ok ? "ok" : "err");
  });

  // Confirm destructive actions before HTX sends the request.
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

  // Refresh relative times every 15s for elements carrying a data-epoch (ms).
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
  setInterval(refreshTimes, 15000);
})();
