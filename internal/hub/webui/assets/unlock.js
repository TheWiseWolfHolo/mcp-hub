(function () {
  "use strict";

  var THEME_KEY = "mcphub_ui_theme";
  var root = document.documentElement;

  function applyStoredTheme() {
    try {
      var saved = localStorage.getItem(THEME_KEY);
      if (saved === "light" || saved === "dark") {
        root.dataset.theme = saved;
      }
    } catch (_) {}
  }

  function currentTheme() {
    if (root.dataset.theme === "dark" || root.dataset.theme === "light") {
      return root.dataset.theme;
    }
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }

  function toggleTheme() {
    var next = currentTheme() === "dark" ? "light" : "dark";
    root.dataset.theme = next;
    try {
      localStorage.setItem(THEME_KEY, next);
    } catch (_) {}
  }

  applyStoredTheme();

  var themeBtn = document.getElementById("btn-theme");
  if (themeBtn) {
    themeBtn.addEventListener("click", toggleTheme);
  }

  var form = document.getElementById("unlock-form");
  var input = document.getElementById("control-token");
  var submitBtn = document.getElementById("btn-unlock");
  var errBox = document.getElementById("unlock-error");

  function showError(msg) {
    if (!errBox) return;
    errBox.textContent = msg;
    errBox.hidden = !msg;
  }

  if (form && input && submitBtn) {
    form.addEventListener("submit", async function (ev) {
      ev.preventDefault();
      var token = input.value.trim();
      if (!token) {
        showError("Enter the control token.");
        return;
      }
      showError("");
      submitBtn.disabled = true;
      submitBtn.textContent = "Unlocking…";
      try {
        var res = await fetch("/ui/api/login", {
          method: "POST",
          credentials: "same-origin",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ token: token })
        });
        var data = await res.json().catch(function () { return {}; });
        if (res.ok && data.ok) {
          window.location.replace("/ui/");
          return;
        }
        if (res.status === 429) {
          var wait = data.retryAfterSec || 60;
          showError("Too many failed attempts. Retry in " + wait + "s.");
        } else {
          showError("Invalid control token.");
        }
      } catch (_) {
        showError("Cannot reach local hub.");
      } finally {
        submitBtn.disabled = false;
        submitBtn.textContent = "Unlock";
      }
    });
  }
})();
