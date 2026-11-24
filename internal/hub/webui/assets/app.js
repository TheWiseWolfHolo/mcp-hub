(function () {
  "use strict";

  var THEME_KEY = "mcphub_ui_theme";
  var MAX_LOG_BUFFER = 1000;
  var SVG_NS = "http://www.w3.org/2000/svg";

  var state = {
    view: "overview",
    csrfToken: "",
    authMode: "token",
    status: null,
    catalog: null,
    catalogMap: {},
    selectedService: null,
    serviceFilter: "all",
    serviceSearch: "",
    snippetClient: "claude",
    snippetCache: {},
    busyActions: {},
    eventSource: null,
    refreshTimer: null,

    logTarget: "hub",
    logSource: null,
    logEntries: [],
    logPaused: false,
    logPending: [],
    logAutoScroll: true,
    logFilter: "",

    analyticsSearch: "",
    analyticsSortKey: "calls",
    analyticsSortDir: "desc",

    explorerService: "",
    explorerResult: null,
    explorerFilter: "",
    explorerOpenTools: {}
  };

  function $(id) {
    return document.getElementById(id);
  }

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }

  function clearNode(node) {
    while (node && node.firstChild) {
      node.removeChild(node.firstChild);
    }
  }

  function svgIcon(name, size) {
    var s = size || 15;
    var svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("viewBox", "0 0 24 24");
    svg.setAttribute("width", String(s));
    svg.setAttribute("height", String(s));
    svg.setAttribute("fill", "none");
    svg.setAttribute("stroke", "currentColor");
    svg.setAttribute("stroke-width", "2");
    svg.setAttribute("stroke-linecap", "round");
    svg.setAttribute("stroke-linejoin", "round");
    svg.setAttribute("aria-hidden", "true");

    function add(tag, attrs) {
      var child = document.createElementNS(SVG_NS, tag);
      for (var k in attrs) {
        if (Object.prototype.hasOwnProperty.call(attrs, k)) {
          child.setAttribute(k, attrs[k]);
        }
      }
      svg.appendChild(child);
    }

    switch (name) {
      case "play":
        add("polygon", { points: "6 4 20 12 6 20 6 4" });
        break;
      case "wake":
        add("polygon", { points: "13 2 3 14 12 14 11 22 21 10 12 10 13 2" });
        break;
      case "stop":
        add("rect", { x: "5", y: "5", width: "14", height: "14", rx: "2" });
        break;
      case "restart":
        add("path", { d: "M3 12a9 9 0 1 0 3-6.7L3 8" });
        add("path", { d: "M3 3v5h5" });
        break;
      case "reload":
        add("path", { d: "M21 12a9 9 0 0 0-9-9 9.75 9.75 0 0 0-6.74 2.74L3 8" });
        add("path", { d: "M3 3v5h5" });
        add("path", { d: "M3 12a9 9 0 0 0 9 9 9.75 9.75 0 0 0 6.74-2.74L21 16" });
        add("path", { d: "M16 16h5v5" });
        break;
      case "sleep":
        add("path", { d: "M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z" });
        break;
      case "chevron":
        add("polyline", { points: "6 9 12 15 18 9" });
        break;
      case "logs":
        add("polyline", { points: "4 17 10 11 4 5" });
        add("line", { x1: "12", y1: "19", x2: "20", y2: "19" });
        break;
      case "tools":
        add("path", { d: "M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z" });
        break;
      case "check":
        add("polyline", { points: "20 6 9 17 4 12" });
        break;
      case "copy":
        add("rect", { x: "9", y: "9", width: "13", height: "13", rx: "2" });
        add("path", { d: "M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" });
        break;
    }
    return svg;
  }

  /* Theme */
  function initTheme() {
    try {
      var saved = localStorage.getItem(THEME_KEY);
      if (saved === "light" || saved === "dark") {
        document.documentElement.dataset.theme = saved;
      }
    } catch (_) {}
  }

  function currentTheme() {
    var t = document.documentElement.dataset.theme;
    if (t === "dark" || t === "light") return t;
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }

  function toggleTheme() {
    var next = currentTheme() === "dark" ? "light" : "dark";
    document.documentElement.dataset.theme = next;
    try {
      localStorage.setItem(THEME_KEY, next);
    } catch (_) {}
  }

  /* Toasts */
  function toast(msg, isError) {
    var stack = $("toast-stack");
    if (!stack) return;
    var item = el("div", "toast " + (isError ? "toast-error" : "toast-ok"), msg);
    stack.appendChild(item);
    window.setTimeout(function () {
      if (item.parentNode === stack) stack.removeChild(item);
    }, isError ? 4200 : 2400);
  }

  /* Clipboard */
  async function copyText(text, label) {
    if (!text) return false;
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        await navigator.clipboard.writeText(text);
      } else {
        var ta = document.createElement("textarea");
        ta.value = text;
        ta.setAttribute("readonly", "");
        document.body.appendChild(ta);
        ta.select();
        document.execCommand("copy");
        document.body.removeChild(ta);
      }
      if (label) toast("Copied " + label);
      return true;
    } catch (_) {
      toast("Clipboard copy failed", true);
      return false;
    }
  }

  /* API helpers */
  async function refreshSessionCSRF() {
    var res = await fetch("/ui/api/session", { credentials: "same-origin" });
    if (res.status === 401) {
      window.location.replace("/ui/");
      return false;
    }
    var data = await res.json().catch(function () { return {}; });
    if (data && data.csrfToken) {
      state.csrfToken = data.csrfToken;
      return true;
    }
    return false;
  }

  async function apiGet(path) {
    var res = await fetch(path, { credentials: "same-origin" });
    if (res.status === 401) {
      window.location.replace("/ui/");
      throw new Error("Session expired");
    }
    var data = await res.json().catch(function () { return {}; });
    if (!res.ok) {
      throw new Error(data.error || ("HTTP " + res.status));
    }
    return data;
  }

  async function apiPost(path, body, retried) {
    var headers = { "X-McpHub-CSRF": state.csrfToken };
    var opts = {
      method: "POST",
      credentials: "same-origin",
      headers: headers
    };
    if (body !== undefined) {
      headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    var res = await fetch(path, opts);
    if (res.status === 401) {
      window.location.replace("/ui/");
      throw new Error("Session expired");
    }
    var data = await res.json().catch(function () { return {}; });
    if (res.status === 403 && data.error === "forbidden_csrf" && !retried) {
      if (await refreshSessionCSRF()) {
        return apiPost(path, body, true);
      }
    }
    if (!res.ok) {
      throw new Error(data.error || ("HTTP " + res.status));
    }
    return data;
  }

  /* Formatting helpers */
  function fmtUptime(sec) {
    if (!sec || sec < 0) return "0s";
    var d = Math.floor(sec / 86400);
    var h = Math.floor((sec % 86400) / 3600);
    var m = Math.floor((sec % 3600) / 60);
    var s = Math.floor(sec % 60);
    if (d > 0) return d + "d " + h + "h";
    if (h > 0) return h + "h " + m + "m";
    if (m > 0) return m + "m " + s + "s";
    return s + "s";
  }

  function fmtMB(val) {
    if (typeof val !== "number" || val <= 0) return "—";
    if (val >= 1024) return (val / 1024).toFixed(2) + " GB";
    return val.toFixed(1) + " MB";
  }

  function fmtInt(n) {
    if (typeof n !== "number" || isNaN(n)) return "0";
    return n.toLocaleString("en-US");
  }

  function fmtRelTime(iso) {
    if (!iso) return "—";
    var ts = Date.parse(iso);
    if (isNaN(ts)) return iso;
    var diff = Math.max(0, Math.floor((Date.now() - ts) / 1000));
    if (diff < 5) return "just now";
    if (diff < 60) return diff + "s ago";
    if (diff < 3600) return Math.floor(diff / 60) + "m ago";
    if (diff < 86400) return Math.floor(diff / 3600) + "h ago";
    return Math.floor(diff / 86400) + "d ago";
  }

  function fmtClock(iso) {
    if (!iso) return "--:--:--";
    var d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    function pad(n) { return n < 10 ? "0" + n : String(n); }
    return pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
  }

  function effectiveState(svc) {
    var st = (svc.state || "").toLowerCase();
    if (st === "degraded") return "degraded";
    if (st === "sleeping") return "sleeping";
    if (st === "starting") return "starting";
    if (st === "stopping") return "starting";
    if (st === "stopped") return "stopped";
    if (svc.running) {
      return svc.healthy ? "healthy" : "degraded";
    }
    return "stopped";
  }

  function stateLabel(svc) {
    var eff = effectiveState(svc);
    if (eff === "degraded" && svc.running && !svc.healthy && svc.state !== "degraded") {
      return "unhealthy";
    }
    return eff;
  }

  function canServiceSleep(svc, cat) {
    if (!svc) return false;
    if (svc.idleSleepSec && svc.idleSleepSec > 0) return true;
    if (svc.startMode === "lazy") return true;
    if (svc.mode === "stdio-bridge") return true;
    if (cat && cat.capabilities && cat.capabilities.lazyWake) return true;
    return false;
  }

  function canServiceReload(svc, cat) {
    if (svc && svc.hasReload) return true;
    if (cat && cat.capabilities && cat.capabilities.reload) return true;
    return false;
  }

  function sortedServiceNames() {
    if (!state.status || !state.status.services) return [];
    return Object.keys(state.status.services).sort();
  }

  /* Data fetch & rendering */
  async function loadDashboardData() {
    try {
      var results = await Promise.all([
        apiGet("/ui/api/status"),
        apiGet("/ui/api/catalog")
      ]);
      state.status = results[0];
      state.catalog = results[1];
      state.catalogMap = {};
      if (state.catalog && Array.isArray(state.catalog.entries)) {
        state.catalog.entries.forEach(function (entry) {
          state.catalogMap[entry.name] = entry;
        });
      }
      renderAll();
    } catch (err) {
      toast("Refresh failed: " + err.message, true);
    }
  }

  function scheduleRefresh() {
    if (state.refreshTimer) return;
    state.refreshTimer = window.setTimeout(function () {
      state.refreshTimer = null;
      loadDashboardData();
    }, 180);
  }

  function renderAll() {
    if (!state.status) return;
    var verEl = $("hub-version");
    if (verEl) {
      verEl.textContent = "v" + (state.status.version || "—");
    }
    renderMetricStrip();
    renderServicesTable();
    renderRoutesTable();
    renderDetailDrawer();
    syncSelectors();
    renderToolAnalytics();
  }

  function renderMetricStrip() {
    var st = state.status;
    var names = sortedServiceNames();
    var total = names.length;
    var healthy = 0;
    var sleeping = 0;
    var stopped = 0;
    var issues = 0;
    var totalReqs = 0;
    var totalErrs = 0;
    var totalSSE = 0;

    names.forEach(function (name) {
      var svc = st.services[name];
      var eff = effectiveState(svc);
      if (eff === "healthy") healthy++;
      else if (eff === "sleeping") sleeping++;
      else if (eff === "degraded") issues++;
      else if (eff === "stopped") stopped++;

      if (svc.metrics) {
        totalReqs += svc.metrics.totalRequests || 0;
        totalErrs += svc.metrics.errorRequests || 0;
        totalSSE += svc.metrics.activeSSE || 0;
      } else if (svc.activeSSE) {
        totalSSE += svc.activeSSE;
      }
    });

    // Include standalone gateway routes / proxies not linked to a managed service
    if (st.gateway && Array.isArray(st.gateway.routes)) {
      st.gateway.routes.forEach(function (r) {
        if (!r.service && r.metrics) {
          totalReqs += r.metrics.totalRequests || 0;
          totalErrs += r.metrics.errorRequests || 0;
          totalSSE += r.metrics.activeSSE || 0;
        }
      });
    }
    if (st.proxies) {
      Object.keys(st.proxies).forEach(function (k) {
        var p = st.proxies[k];
        if (!p.upstreamService && p.metrics) {
          totalReqs += p.metrics.totalRequests || 0;
          totalErrs += p.metrics.errorRequests || 0;
          totalSSE += p.metrics.activeSSE || 0;
        }
      });
    }

    $("metric-healthy").textContent = healthy + " / " + total;
    var issuesChip = $("metric-issues-chip");
    if (issues > 0) {
      issuesChip.textContent = issues + " degraded";
      issuesChip.hidden = false;
    } else {
      issuesChip.hidden = true;
    }

    $("metric-sleeping").textContent = String(sleeping);
    $("metric-stopped-sub").textContent = stopped > 0 ? (stopped + " stopped") : "";

    var privMB = st.treeTotalPrivateMB || 0;
    var wsMB = st.treeTotalWorkingSetMB || 0;
    $("metric-ram").textContent = privMB > 0 ? fmtMB(privMB) : "0.0 MB";
    $("metric-ws-sub").textContent = wsMB > 0 ? ("/ " + fmtMB(wsMB) + " WS") : "";

    $("metric-uptime").textContent = fmtUptime(st.uptimeSec || 0);
    $("metric-pid-sub").textContent = st.pid ? ("PID " + st.pid) : "";

    $("metric-requests").textContent = fmtInt(totalReqs);
    var sseParts = [];
    sseParts.push(totalSSE + " SSE");
    if (totalErrs > 0) sseParts.push(fmtInt(totalErrs) + " err");
    $("metric-sse-sub").textContent = sseParts.join(" · ");
  }

  function matchesServiceFilter(svc, name) {
    var eff = effectiveState(svc);
    if (state.serviceFilter === "running" && eff !== "healthy" && eff !== "starting") return false;
    if (state.serviceFilter === "sleeping" && eff !== "sleeping") return false;
    if (state.serviceFilter === "issues" && eff !== "degraded" && eff !== "stopped") return false;

    if (state.serviceSearch) {
      var q = state.serviceSearch.toLowerCase();
      var hay = [name, svc.mode || "", svc.state || "", svc.description || "", (svc.tags || []).join(" ")].join(" ").toLowerCase();
      if (hay.indexOf(q) === -1) return false;
    }
    return true;
  }

  function renderServicesTable() {
    var tbody = $("services-tbody");
    var emptyEl = $("services-empty");
    clearNode(tbody);

    var names = sortedServiceNames();
    var visible = 0;

    names.forEach(function (name) {
      var svc = state.status.services[name];
      if (!matchesServiceFilter(svc, name)) return;
      visible++;

      var cat = state.catalogMap[name];
      var eff = effectiveState(svc);
      var lbl = stateLabel(svc);

      var tr = document.createElement("tr");
      if (state.selectedService === name) tr.className = "is-selected";
      tr.dataset.service = name;
      tr.addEventListener("click", function () {
        selectService(state.selectedService === name ? null : name);
      });

      // 1. Service name + dot + mode badge
      var tdName = document.createElement("td");
      var svcCell = el("div", "svc-cell");
      svcCell.appendChild(el("span", "status-dot dot-" + eff));
      svcCell.appendChild(el("span", "svc-name", name));
      var modeLabel = svc.mode === "stdio-bridge" ? "stdio" : "http";
      svcCell.appendChild(el("span", "badge mono " + (modeLabel === "http" ? "badge-http" : "badge-stdio"), modeLabel));
      tdName.appendChild(svcCell);
      tr.appendChild(tdName);

      // 2. State pill
      var tdState = document.createElement("td");
      tdState.appendChild(el("span", "state-pill state-" + eff, lbl));
      tr.appendChild(tdState);

      // 3. PID / Port
      var tdPid = el("td", "mono");
      if (svc.pid > 0 && svc.port > 0) {
        tdPid.textContent = svc.pid + " · :" + svc.port;
      } else if (svc.pid > 0) {
        tdPid.textContent = String(svc.pid);
      } else if (svc.port > 0) {
        tdPid.className = "mono cell-muted";
        tdPid.textContent = ":" + svc.port;
      } else {
        tdPid.className = "mono cell-muted";
        tdPid.textContent = "—";
      }
      tr.appendChild(tdPid);

      // 4. RAM
      var tdRam = el("td", "mono num");
      if (svc.treePrivateMB > 0) {
        tdRam.textContent = fmtMB(svc.treePrivateMB);
      } else {
        tdRam.className = "mono num cell-muted";
        tdRam.textContent = "—";
      }
      tr.appendChild(tdRam);

      // 5. Traffic
      var tdTraffic = el("td", "mono");
      var m = svc.metrics;
      var reqs = m ? (m.totalRequests || 0) : 0;
      var errs = m ? (m.errorRequests || 0) : 0;
      var sse = m ? (m.activeSSE || 0) : (svc.activeSSE || 0);
      if (reqs > 0 || errs > 0 || sse > 0) {
        var tWrap = el("div", "traffic-inline");
        tWrap.appendChild(el("span", "", fmtInt(reqs) + " req"));
        if (errs > 0) {
          tWrap.appendChild(el("span", "cell-error", errs + " err"));
        }
        if (sse > 0) {
          tWrap.appendChild(el("span", "badge badge-sse", sse + " SSE"));
        }
        tdTraffic.appendChild(tWrap);
      } else {
        tdTraffic.className = "mono cell-muted";
        tdTraffic.textContent = "—";
      }
      tr.appendChild(tdTraffic);

      // 6. Restarts (Crash Restarts vs Idle Wakeups)
      var tdRestarts = el("td", "mono");
      var crashes = svc.crashRestarts || 0;
      var wakes = svc.wakeCount || 0;
      var legacyRestarts = svc.restarts || 0;
      if (crashes > 0 || wakes > 0) {
        var rWrap = el("div", "restart-inline");
        if (crashes > 0) {
          rWrap.appendChild(el("span", "crash-badge", crashes + (crashes === 1 ? " crash" : " crashes")));
        }
        if (wakes > 0) {
          rWrap.appendChild(el("span", "wake-text", wakes + (wakes === 1 ? " wake" : " wakes")));
        }
        tdRestarts.appendChild(rWrap);
      } else if (legacyRestarts > 0) {
        tdRestarts.className = "mono cell-muted";
        tdRestarts.textContent = legacyRestarts + (legacyRestarts === 1 ? " restart" : " restarts");
      } else {
        tdRestarts.className = "mono cell-muted";
        tdRestarts.textContent = "—";
      }
      tr.appendChild(tdRestarts);

      // 7. Quick Actions
      var tdAct = el("td", "actions-col");
      var actWrap = el("div", "row-actions");
      buildQuickActionButtons(actWrap, name, svc, cat);
      tdAct.appendChild(actWrap);
      tr.appendChild(tdAct);

      tbody.appendChild(tr);
    });

    $("services-count").textContent = String(visible);
    emptyEl.hidden = visible > 0;
  }

  function makeTableActionBtn(iconName, title, isDanger, disabled, onClick) {
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "icon-btn icon-btn-table" + (isDanger ? " icon-btn-danger" : "");
    btn.title = title;
    btn.setAttribute("aria-label", title);
    btn.disabled = !!disabled;
    btn.appendChild(svgIcon(iconName, 14));
    btn.addEventListener("click", function (ev) {
      ev.stopPropagation();
      onClick();
    });
    return btn;
  }

  function buildQuickActionButtons(container, name, svc, cat) {
    var eff = effectiveState(svc);
    var busy = !!state.busyActions[name];

    if (eff === "sleeping") {
      container.appendChild(makeTableActionBtn("wake", "Wake " + name, false, busy, function () {
        runServiceAction(name, "wake");
      }));
      container.appendChild(makeTableActionBtn("stop", "Stop " + name, true, busy, function () {
        runServiceAction(name, "stop");
      }));
      return;
    }

    if (eff === "stopped") {
      container.appendChild(makeTableActionBtn("play", "Start " + name, false, busy, function () {
        runServiceAction(name, "start");
      }));
      return;
    }

    if (eff === "degraded" && !svc.running) {
      container.appendChild(makeTableActionBtn("play", "Start " + name, false, busy, function () {
        runServiceAction(name, "start");
      }));
      container.appendChild(makeTableActionBtn("restart", "Restart " + name, false, busy, function () {
        runServiceAction(name, "restart");
      }));
      return;
    }

    // Running or starting
    container.appendChild(makeTableActionBtn("restart", "Restart " + name, false, busy, function () {
      runServiceAction(name, "restart");
    }));
    if (canServiceReload(svc, cat)) {
      container.appendChild(makeTableActionBtn("reload", "Reload " + name, false, busy, function () {
        runServiceAction(name, "reload");
      }));
    }
    if (canServiceSleep(svc, cat)) {
      container.appendChild(makeTableActionBtn("sleep", "Sleep " + name, false, busy, function () {
        runServiceAction(name, "sleep");
      }));
    }
    container.appendChild(makeTableActionBtn("stop", "Stop " + name, true, busy, function () {
      runServiceAction(name, "stop");
    }));
  }

  function renderRoutesTable() {
    var tbody = $("routes-tbody");
    var emptyEl = $("routes-empty");
    clearNode(tbody);

    var gwAddrEl = $("gateway-listen-addr");
    var gw = state.status.gateway;
    var proxies = state.status.proxies || {};
    var count = 0;

    if (gw && gw.listenAddr) {
      gwAddrEl.textContent = "gateway " + gw.listenAddr;
    } else {
      gwAddrEl.textContent = "";
    }

    if (gw && Array.isArray(gw.routes)) {
      gw.routes.forEach(function (route) {
        count++;
        var tr = document.createElement("tr");

        var tdSlug = el("td", "mono");
        tdSlug.textContent = "/s/" + route.slug;
        tr.appendChild(tdSlug);

        var tdMode = document.createElement("td");
        var modeLbl = route.mode === "stdio-bridge" ? "stdio" : "http";
        tdMode.appendChild(el("span", "badge mono " + (modeLbl === "http" ? "badge-http" : "badge-stdio"), modeLbl));
        tr.appendChild(tdMode);

        var tdTarget = el("td", "mono cell-muted");
        tdTarget.textContent = route.service || route.targetUrl || "—";
        tr.appendChild(tdTarget);

        var tdState = document.createElement("td");
        var rState = (route.state || (gw.running ? "healthy" : "stopped")).toLowerCase();
        var dotCls = rState === "running" || rState === "healthy" || rState === "external" ? "healthy"
          : rState === "sleeping" ? "sleeping"
          : rState === "degraded" ? "degraded" : "stopped";
        var sWrap = el("div", "svc-cell");
        sWrap.appendChild(el("span", "status-dot dot-" + dotCls));
        sWrap.appendChild(el("span", "cell-muted", rState));
        tdState.appendChild(sWrap);
        tr.appendChild(tdState);

        var m = route.metrics;
        var tdSess = el("td", "mono num", m ? fmtInt(m.activeSessions || 0) : "0");
        tr.appendChild(tdSess);

        var tdTr = el("td", "mono num");
        if (m && (m.totalRequests > 0 || m.errorRequests > 0)) {
          tdTr.appendChild(document.createTextNode(fmtInt(m.totalRequests) + " req"));
          if (m.errorRequests > 0) {
            tdTr.appendChild(document.createTextNode(" · "));
            tdTr.appendChild(el("span", "cell-error", m.errorRequests + " err"));
          }
        } else {
          tdTr.className = "mono num cell-muted";
          tdTr.textContent = "—";
        }
        tr.appendChild(tdTr);

        tbody.appendChild(tr);
      });
    }

    Object.keys(proxies).sort().forEach(function (name) {
      var p = proxies[name];
      count++;
      var tr = document.createElement("tr");

      var tdName = el("td", "mono");
      tdName.textContent = name + " (" + p.listenAddr + ")";
      tr.appendChild(tdName);

      var tdMode = document.createElement("td");
      tdMode.appendChild(el("span", "badge mono", "proxy"));
      tr.appendChild(tdMode);

      var tdTarget = el("td", "mono cell-muted");
      tdTarget.textContent = p.upstreamService ? (p.upstreamService + " → " + p.targetUrl) : p.targetUrl;
      tr.appendChild(tdTarget);

      var tdState = document.createElement("td");
      var sWrap = el("div", "svc-cell");
      sWrap.appendChild(el("span", "status-dot " + (p.running ? "dot-healthy" : "dot-stopped")));
      sWrap.appendChild(el("span", "cell-muted", p.running ? "running" : "stopped"));
      tdState.appendChild(sWrap);
      tr.appendChild(tdState);

      var m = p.metrics;
      tr.appendChild(el("td", "mono num", m ? fmtInt(m.activeSessions || 0) : "0"));

      var tdTr = el("td", "mono num");
      if (m && (m.totalRequests > 0 || m.errorRequests > 0)) {
        tdTr.appendChild(document.createTextNode(fmtInt(m.totalRequests) + " req"));
        if (m.errorRequests > 0) {
          tdTr.appendChild(document.createTextNode(" · "));
          tdTr.appendChild(el("span", "cell-error", m.errorRequests + " err"));
        }
      } else {
        tdTr.className = "mono num cell-muted";
        tdTr.textContent = "—";
      }
      tr.appendChild(tdTr);

      tbody.appendChild(tr);
    });

    $("routes-count").textContent = String(count);
    emptyEl.hidden = count > 0;
  }

  /* Detail Drawer */
  function selectService(name) {
    state.selectedService = name;
    renderServicesTable();
    renderDetailDrawer();
    if (name) {
      loadSnippetForDrawer(name, state.snippetClient);
    }
  }

  function renderDetailDrawer() {
    var split = $("overview-split");
    var drawer = $("detail-drawer");
    var name = state.selectedService;
    if (!name || !state.status || !state.status.services || !state.status.services[name]) {
      split.classList.remove("has-drawer");
      drawer.hidden = true;
      return;
    }

    var svc = state.status.services[name];
    var cat = state.catalogMap[name];
    var eff = effectiveState(svc);
    var busy = !!state.busyActions[name];

    split.classList.add("has-drawer");
    drawer.hidden = false;

    $("drawer-dot").className = "status-dot dot-" + eff;
    $("drawer-name").textContent = name;
    var modeEl = $("drawer-mode");
    var modeLbl = svc.mode === "stdio-bridge" ? "stdio" : "http";
    modeEl.textContent = modeLbl;
    modeEl.className = "badge mono " + (modeLbl === "http" ? "badge-http" : "badge-stdio");
    $("drawer-start-mode").textContent = svc.startMode || "eager";

    // Drawer action bar
    var actionsBox = $("drawer-actions");
    clearNode(actionsBox);
    var lifeGroup = el("div", "drawer-actions-group");
    var navGroup = el("div", "drawer-actions-nav");

    function addActionBtn(label, iconName, cls, fn) {
      var b = el("button", "btn btn-sm " + cls);
      b.type = "button";
      b.disabled = busy;
      b.appendChild(svgIcon(iconName, 13));
      b.appendChild(el("span", "", label));
      b.addEventListener("click", fn);
      lifeGroup.appendChild(b);
    }

    if (eff === "sleeping") {
      addActionBtn("Wake", "wake", "btn-primary", function () { runServiceAction(name, "wake"); });
      addActionBtn("Stop", "stop", "btn-danger", function () { runServiceAction(name, "stop"); });
    } else if (eff === "stopped") {
      addActionBtn("Start", "play", "btn-primary", function () { runServiceAction(name, "start"); });
    } else if (eff === "degraded" && !svc.running) {
      addActionBtn("Start", "play", "btn-primary", function () { runServiceAction(name, "start"); });
      addActionBtn("Restart", "restart", "btn-secondary", function () { runServiceAction(name, "restart"); });
    } else {
      addActionBtn("Restart", "restart", "btn-secondary", function () { runServiceAction(name, "restart"); });
      if (canServiceReload(svc, cat)) {
        addActionBtn("Reload", "reload", "btn-secondary", function () { runServiceAction(name, "reload"); });
      }
      if (canServiceSleep(svc, cat)) {
        addActionBtn("Sleep", "sleep", "btn-secondary", function () { runServiceAction(name, "sleep"); });
      }
      addActionBtn("Stop", "stop", "btn-danger", function () { runServiceAction(name, "stop"); });
    }

    var logsJump = el("button", "btn btn-ghost btn-sm");
    logsJump.type = "button";
    logsJump.appendChild(svgIcon("logs", 13));
    logsJump.appendChild(el("span", "", "Logs"));
    logsJump.addEventListener("click", function () {
      openServiceLogs(name);
    });
    navGroup.appendChild(logsJump);

    var toolsJump = el("button", "btn btn-ghost btn-sm");
    toolsJump.type = "button";
    toolsJump.appendChild(svgIcon("tools", 13));
    toolsJump.appendChild(el("span", "", "Tools"));
    toolsJump.addEventListener("click", function () {
      openServiceTools(name);
    });
    navGroup.appendChild(toolsJump);

    actionsBox.appendChild(lifeGroup);
    actionsBox.appendChild(navGroup);

    // Error & Crash Tail
    var errMsg = svc.lastError || svc.lastHealthError || "";
    var crashLines = Array.isArray(svc.crashTail) ? svc.crashTail : [];
    var errSec = $("drawer-error-section");
    if (errMsg || crashLines.length > 0) {
      errSec.hidden = false;
      $("drawer-last-error").textContent = errMsg || "Service reported recent crash output.";
      var crashWrap = $("drawer-crash-wrap");
      if (crashLines.length > 0) {
        crashWrap.hidden = false;
        $("drawer-crash-tail").textContent = crashLines.join("\n");
      } else {
        crashWrap.hidden = true;
      }
    } else {
      errSec.hidden = true;
    }

    // Endpoint & Auth
    var endpoint = (cat && cat.clientUrl) ? cat.clientUrl : (svc.port > 0 ? ("http://127.0.0.1:" + svc.port + "/mcp") : "—");
    $("drawer-endpoint").textContent = endpoint;
    $("btn-copy-endpoint").disabled = (endpoint === "—");

    var authEl = $("drawer-auth");
    clearNode(authEl);
    if (cat && cat.auth && cat.auth.type === "bearer") {
      authEl.appendChild(el("span", "badge mono", "Bearer"));
      if (cat.auth.tokenFile) {
        authEl.appendChild(document.createTextNode(" "));
        authEl.appendChild(el("span", "mono", cat.auth.tokenFile));
        authEl.title = "Bearer · " + cat.auth.tokenFile;
      } else {
        authEl.title = "Bearer";
      }
    } else {
      authEl.textContent = "None";
      authEl.title = "None";
    }

    // Metrics KV Grid
    var grid = $("drawer-metrics-grid");
    clearNode(grid);
    var m = svc.metrics || {};
    var kvs = [
      ["PID / Port", (svc.pid > 0 ? svc.pid : "—") + (svc.port > 0 ? (" / :" + svc.port) : "")],
      ["RAM (Priv / WS)", (svc.treePrivateMB > 0 ? fmtMB(svc.treePrivateMB) : "—") + (svc.treeWorkingSetMB > 0 ? (" / " + fmtMB(svc.treeWorkingSetMB)) : "")],
      ["Requests / Errors", fmtInt(m.totalRequests || 0) + " / " + fmtInt(m.errorRequests || 0)],
      ["Active SSE / Sessions", fmtInt(m.activeSSE || svc.activeSSE || 0) + " / " + fmtInt(m.activeSessions || 0)],
      ["Crashes / Wakes", fmtInt(svc.crashRestarts || 0) + " / " + fmtInt(svc.wakeCount || 0)],
      ["Last Tool", m.lastTool || "—"]
    ];
    kvs.forEach(function (pair) {
      var item = el("div", "kv-item");
      item.appendChild(el("div", "kv-key", pair[0]));
      var v = el("div", "kv-val mono", pair[1]);
      v.title = pair[1];
      item.appendChild(v);
      grid.appendChild(item);
    });

    // Update snippet active tab & cached content
    document.querySelectorAll("#snippet-tabs .snippet-tab").forEach(function (tab) {
      tab.classList.toggle("is-active", tab.dataset.client === state.snippetClient);
    });
    var cacheKey = name + "::" + state.snippetClient;
    if (state.snippetCache[cacheKey]) {
      $("drawer-snippet").textContent = state.snippetCache[cacheKey];
    }
  }

  async function loadSnippetForDrawer(service, client) {
    var pre = $("drawer-snippet");
    var cacheKey = service + "::" + client;
    if (state.snippetCache[cacheKey]) {
      pre.textContent = state.snippetCache[cacheKey];
      return;
    }
    pre.textContent = "Loading…";
    try {
      var data = await apiGet("/ui/api/snippet?service=" + encodeURIComponent(service) + "&client=" + encodeURIComponent(client));
      var text = data.snippet || "";
      state.snippetCache[cacheKey] = text;
      if (state.selectedService === service && state.snippetClient === client) {
        pre.textContent = text;
      }
    } catch (err) {
      if (state.selectedService === service && state.snippetClient === client) {
        pre.textContent = "Unavailable: " + err.message;
      }
    }
  }

  /* Service & Hub Actions */
  async function runServiceAction(name, action) {
    state.busyActions[name] = true;
    renderServicesTable();
    renderDetailDrawer();
    try {
      var path = "/ui/api/services/" + encodeURIComponent(name) + "/" + action;
      if (action === "restart") path += "?wait=healthy";
      await apiPost(path);
      toast(action.charAt(0).toUpperCase() + action.slice(1) + ": " + name);
      await loadDashboardData();
    } catch (err) {
      toast(name + " " + action + " failed: " + err.message, true);
    } finally {
      delete state.busyActions[name];
      renderServicesTable();
      renderDetailDrawer();
    }
  }

  async function runGlobalAction(btnId, path, label) {
    var btn = $(btnId);
    if (btn) btn.disabled = true;
    try {
      await apiPost(path);
      toast(label + " complete");
      await loadDashboardData();
    } catch (err) {
      toast(label + " failed: " + err.message, true);
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  /* Live SSE Events */
  function connectEventStream() {
    if (state.eventSource) {
      state.eventSource.close();
      state.eventSource = null;
    }
    setSSEStatus("muted", "Connecting");
    var es = new EventSource("/ui/api/events");
    state.eventSource = es;

    es.addEventListener("ready", function () {
      setSSEStatus("ok", "Live");
    });
    es.onopen = function () {
      setSSEStatus("ok", "Live");
    };
    es.onmessage = function () {
      scheduleRefresh();
    };
    ["state_change", "service_change", "reload", "service_started", "service_stopped", "service_sleeping", "service_woken", "service_degraded"].forEach(function (evName) {
      es.addEventListener(evName, function () {
        scheduleRefresh();
      });
    });
    es.onerror = function () {
      setSSEStatus("warn", "Reconnecting");
    };
  }

  function setSSEStatus(dotState, label) {
    var dot = $("sse-dot");
    var txt = $("sse-label");
    if (dot) dot.className = "status-dot dot-" + dotState;
    if (txt) txt.textContent = label;
  }

  /* Selectors sync */
  function syncSelectors() {
    var names = sortedServiceNames();

    // Log target select
    var logSel = $("log-target-select");
    if (logSel) {
      var prevLog = state.logTarget || "hub";
      clearNode(logSel);
      var hubOpt = document.createElement("option");
      hubOpt.value = "hub";
      hubOpt.textContent = "hub";
      logSel.appendChild(hubOpt);
      names.forEach(function (n) {
        var o = document.createElement("option");
        o.value = n;
        o.textContent = n;
        logSel.appendChild(o);
      });
      logSel.value = (prevLog === "hub" || names.indexOf(prevLog) >= 0) ? prevLog : "hub";
      state.logTarget = logSel.value;
    }

    // Explorer service select
    var expSel = $("explorer-service-select");
    if (expSel) {
      var prevExp = state.explorerService;
      clearNode(expSel);
      names.forEach(function (n) {
        var svc = state.status.services[n];
        var o = document.createElement("option");
        o.value = n;
        o.textContent = n + " (" + stateLabel(svc) + ")";
        expSel.appendChild(o);
      });
      if (names.length > 0) {
        expSel.value = names.indexOf(prevExp) >= 0 ? prevExp : names[0];
        state.explorerService = expSel.value;
      } else {
        state.explorerService = "";
      }
      $("btn-inspect-tools").disabled = !state.explorerService;
    }
  }

  /* View Switching */
  function switchView(viewName) {
    state.view = viewName;
    document.querySelectorAll(".nav-tab").forEach(function (btn) {
      var active = btn.dataset.view === viewName;
      btn.classList.toggle("is-active", active);
      btn.setAttribute("aria-selected", active ? "true" : "false");
    });
    ["overview", "logs", "tools"].forEach(function (v) {
      var panel = $("view-" + v);
      if (panel) panel.hidden = (v !== viewName);
    });

    if (viewName === "logs") {
      startLogStream(state.logTarget);
    } else {
      stopLogStream();
    }
  }

  function openServiceLogs(name) {
    state.logTarget = name;
    var sel = $("log-target-select");
    if (sel) sel.value = name;
    switchView("logs");
  }

  function openServiceTools(name) {
    state.explorerService = name;
    var sel = $("explorer-service-select");
    if (sel) sel.value = name;
    switchView("tools");
  }

  /* Live Logs */
  function setLogStreamBadge(dotState, label) {
    $("log-stream-dot").className = "status-dot dot-" + dotState;
    $("log-stream-label").textContent = label;
  }

  function stopLogStream() {
    if (state.logSource) {
      state.logSource.close();
      state.logSource = null;
    }
    setLogStreamBadge("muted", "Idle");
  }

  function startLogStream(target) {
    stopLogStream();
    state.logTarget = target || "hub";
    state.logEntries = [];
    state.logPending = [];
    state.logPaused = false;
    $("btn-log-pause").textContent = "Pause";
    renderLogs();
    setLogStreamBadge("warn", "Connecting");

    var url = "/ui/api/logs/" + encodeURIComponent(state.logTarget) + "?follow=1&lines=150";
    var es = new EventSource(url);
    state.logSource = es;

    es.onopen = function () {
      setLogStreamBadge("ok", "Streaming");
    };
    es.addEventListener("log", function (ev) {
      try {
        var entry = JSON.parse(ev.data);
        if (state.logPaused) {
          state.logPending.push(entry);
          if (state.logPending.length > MAX_LOG_BUFFER) {
            state.logPending.shift();
          }
          setLogStreamBadge("warn", "Paused (+" + state.logPending.length + ")");
          return;
        }
        appendLogEntry(entry);
      } catch (_) {}
    });
    es.onerror = function () {
      if (!state.logPaused) {
        setLogStreamBadge("warn", "Reconnecting");
      }
    };
  }

  function appendLogEntry(entry) {
    state.logEntries.push(entry);
    if (state.logEntries.length > MAX_LOG_BUFFER) {
      state.logEntries.shift();
    }
    var vp = $("log-viewport");
    if (matchesLogFilter(entry)) {
      vp.appendChild(buildLogRow(entry));
      while (vp.childNodes.length > MAX_LOG_BUFFER) {
        vp.removeChild(vp.firstChild);
      }
      if (state.logAutoScroll) {
        vp.scrollTop = vp.scrollHeight;
      }
    }
    updateLogCount();
  }

  function matchesLogFilter(entry) {
    if (!state.logFilter) return true;
    var q = state.logFilter.toLowerCase();
    var hay = ((entry.timestamp || "") + " " + (entry.stream || "") + " " + (entry.line || "")).toLowerCase();
    return hay.indexOf(q) !== -1;
  }

  function renderHighlightedLogText(container, text) {
    var re = /(\[REDACTED\]|\b(?:FATAL|PANIC|ERROR|ERR)\b|\b(?:WARN|WARNING)\b)/gi;
    var lastIndex = 0;
    var match;
    while ((match = re.exec(text)) !== null) {
      if (match.index > lastIndex) {
        container.appendChild(document.createTextNode(text.slice(lastIndex, match.index)));
      }
      var tok = match[0];
      var upper = tok.toUpperCase();
      var cls = upper === "[REDACTED]" ? "tok-redacted"
        : (upper === "WARN" || upper === "WARNING") ? "tok-warn"
        : "tok-err";
      container.appendChild(el("span", cls, tok));
      lastIndex = match.index + tok.length;
    }
    if (lastIndex < text.length) {
      container.appendChild(document.createTextNode(text.slice(lastIndex)));
    }
  }

  function buildLogRow(entry) {
    var row = el("div", "log-row");
    var ts = el("span", "log-ts", fmtClock(entry.timestamp));
    if (entry.timestamp) ts.title = entry.timestamp;
    row.appendChild(ts);

    var streamName = (entry.stream || "log").toLowerCase();
    var streamCls = streamName === "hub" ? "stream-hub"
      : streamName === "stderr" ? "stream-stderr" : "stream-stdout";
    row.appendChild(el("span", "log-stream " + streamCls, "[" + streamName + "]"));

    var msg = el("span", "log-msg");
    renderHighlightedLogText(msg, entry.line || "");
    row.appendChild(msg);
    return row;
  }

  function renderLogs() {
    var vp = $("log-viewport");
    clearNode(vp);
    var frag = document.createDocumentFragment();
    state.logEntries.forEach(function (entry) {
      if (matchesLogFilter(entry)) {
        frag.appendChild(buildLogRow(entry));
      }
    });
    vp.appendChild(frag);
    if (state.logAutoScroll) {
      vp.scrollTop = vp.scrollHeight;
    }
    updateLogCount();
  }

  function updateLogCount() {
    var total = state.logEntries.length;
    var visible = $("log-viewport").childNodes.length;
    $("log-line-count").textContent = state.logFilter ? (visible + " / " + total + " lines") : (total + " lines");
  }

  /* Tool Analytics */
  function collectToolRows() {
    var rows = [];
    if (!state.status || !state.status.services) return rows;
    Object.keys(state.status.services).sort().forEach(function (svcName) {
      var svc = state.status.services[svcName];
      var tools = svc.metrics && svc.metrics.tools;
      if (!tools) return;
      Object.keys(tools).forEach(function (toolName) {
        var tm = tools[toolName];
        rows.push({
          service: svcName,
          tool: toolName,
          calls: tm.calls || 0,
          errors: tm.errors || 0,
          avgMs: typeof tm.avgMs === "number" ? tm.avgMs : 0,
          lastCalledAt: tm.lastCalledAt || ""
        });
      });
    });
    return rows;
  }

  function renderToolAnalytics() {
    var tbody = $("analytics-tbody");
    var emptyEl = $("analytics-empty");
    clearNode(tbody);

    var rows = collectToolRows();
    if (state.analyticsSearch) {
      var q = state.analyticsSearch.toLowerCase();
      rows = rows.filter(function (r) {
        return r.service.toLowerCase().indexOf(q) !== -1 || r.tool.toLowerCase().indexOf(q) !== -1;
      });
    }

    var key = state.analyticsSortKey;
    var dir = state.analyticsSortDir === "asc" ? 1 : -1;
    rows.sort(function (a, b) {
      var av = a[key];
      var bv = b[key];
      if (typeof av === "number" && typeof bv === "number") {
        if (av !== bv) return (av - bv) * dir;
      } else {
        var cmp = String(av).localeCompare(String(bv));
        if (cmp !== 0) return cmp * dir;
      }
      return a.tool.localeCompare(b.tool);
    });

    document.querySelectorAll(".analytics-table .sort-btn").forEach(function (btn) {
      btn.classList.remove("is-sorted-asc", "is-sorted-desc");
      if (btn.dataset.sort === key) {
        btn.classList.add(state.analyticsSortDir === "asc" ? "is-sorted-asc" : "is-sorted-desc");
      }
    });

    rows.forEach(function (r) {
      var tr = document.createElement("tr");
      tr.appendChild(el("td", "cell-muted", r.service));
      tr.appendChild(el("td", "mono tool-name-cell", r.tool));
      tr.appendChild(el("td", "mono num", fmtInt(r.calls)));

      var tdErr = el("td", "mono num " + (r.errors > 0 ? "cell-error" : "cell-muted"), fmtInt(r.errors));
      tr.appendChild(tdErr);

      tr.appendChild(el("td", "mono num", r.avgMs.toFixed(1)));

      var tdLast = el("td", "mono num cell-muted", fmtRelTime(r.lastCalledAt));
      if (r.lastCalledAt) tdLast.title = r.lastCalledAt;
      tr.appendChild(tdLast);

      tbody.appendChild(tr);
    });

    $("analytics-count").textContent = rows.length + (rows.length === 1 ? " tool" : " tools");
    emptyEl.hidden = rows.length > 0;
  }

  /* Live MCP Tools Explorer */
  async function inspectSelectedService() {
    var service = $("explorer-service-select").value;
    if (!service) return;
    var wake = $("explorer-wake-checkbox").checked;
    var btn = $("btn-inspect-tools");
    btn.disabled = true;
    btn.textContent = "Inspecting…";
    $("explorer-notice").hidden = true;

    try {
      var data = await apiPost("/ui/api/probe-tools", { service: service, wake: wake });
      state.explorerResult = data;
      state.explorerOpenTools = {};
      renderExplorerResult();
      if (wake) scheduleRefresh();
    } catch (err) {
      state.explorerResult = null;
      $("explorer-meta").hidden = true;
      $("explorer-count").hidden = true;
      var notice = $("explorer-notice");
      notice.className = "explorer-notice is-error";
      notice.textContent = "Probe failed: " + err.message;
      notice.hidden = false;
      clearNode($("explorer-list"));
    } finally {
      btn.disabled = false;
      btn.textContent = "Inspect Tools";
    }
  }

  function formatSchemaJSON(raw) {
    if (!raw) return "{}";
    try {
      if (typeof raw === "string") {
        return JSON.stringify(JSON.parse(raw), null, 2);
      }
      return JSON.stringify(raw, null, 2);
    } catch (_) {
      return String(raw);
    }
  }

  function renderExplorerResult() {
    var res = state.explorerResult;
    var list = $("explorer-list");
    var meta = $("explorer-meta");
    var notice = $("explorer-notice");
    var countPill = $("explorer-count");
    clearNode(list);

    if (!res) {
      meta.hidden = true;
      notice.hidden = true;
      countPill.hidden = true;
      list.appendChild(el("div", "empty-state", "Select a service and click Inspect Tools."));
      return;
    }

    if (res.skipped) {
      meta.hidden = true;
      countPill.hidden = true;
      notice.className = "explorer-notice";
      notice.textContent = "Skipped: " + (res.reason || "service is sleeping") + ".";
      notice.hidden = false;
      list.appendChild(el("div", "empty-state", "Enable \"Wake if sleeping\" to start and inspect this service."));
      return;
    }

    notice.hidden = true;
    meta.hidden = false;
    $("explorer-endpoint").textContent = res.endpoint || res.service;
    $("explorer-latency").textContent = (res.latencyMs || 0) + " ms";

    var tools = Array.isArray(res.tools) ? res.tools : [];
    var q = state.explorerFilter.toLowerCase();
    var filtered = q ? tools.filter(function (t) {
      return (t.name || "").toLowerCase().indexOf(q) !== -1 || (t.description || "").toLowerCase().indexOf(q) !== -1;
    }) : tools;

    countPill.textContent = filtered.length + (filtered.length === 1 ? " tool" : " tools");
    countPill.hidden = false;

    if (filtered.length === 0) {
      list.appendChild(el("div", "empty-state", tools.length === 0 ? "Service exposed 0 tools." : "No matching tools."));
      return;
    }

    filtered.forEach(function (tool) {
      var isOpen = !!state.explorerOpenTools[tool.name];
      var card = el("div", "tool-card" + (isOpen ? " is-open" : ""));

      var head = document.createElement("button");
      head.type = "button";
      head.className = "tool-card-head";
      head.setAttribute("aria-expanded", isOpen ? "true" : "false");

      var main = el("div", "tool-card-main");
      var titleRow = el("div", "tool-card-title-row");
      titleRow.appendChild(el("span", "tool-card-name mono", tool.name));
      if (tool.readOnlyHint === true) {
        titleRow.appendChild(el("span", "badge badge-readonly", "read-only"));
      } else if (tool.readOnlyHint === false) {
        titleRow.appendChild(el("span", "badge", "write"));
      }
      main.appendChild(titleRow);
      if (tool.description) {
        main.appendChild(el("div", "tool-card-desc", tool.description));
      }
      head.appendChild(main);

      var chev = svgIcon("chevron", 15);
      chev.setAttribute("class", "tool-chevron");
      head.appendChild(chev);

      var body = el("div", "tool-card-body");
      body.hidden = !isOpen;
      var schemaBar = el("div", "schema-bar");
      schemaBar.appendChild(el("span", "section-label", "Input Schema"));
      var schemaStr = formatSchemaJSON(tool.inputSchema);
      var copyBtn = el("button", "btn btn-ghost btn-xs", "Copy JSON");
      copyBtn.type = "button";
      copyBtn.addEventListener("click", function () {
        copyText(schemaStr, tool.name + " schema");
      });
      schemaBar.appendChild(copyBtn);
      body.appendChild(schemaBar);
      body.appendChild(el("pre", "code-block schema-pre mono", schemaStr));

      head.addEventListener("click", function () {
        var nowOpen = !state.explorerOpenTools[tool.name];
        state.explorerOpenTools[tool.name] = nowOpen;
        card.classList.toggle("is-open", nowOpen);
        head.setAttribute("aria-expanded", nowOpen ? "true" : "false");
        body.hidden = !nowOpen;
      });

      card.appendChild(head);
      card.appendChild(body);
      list.appendChild(card);
    });
  }

  /* Event bindings */
  function bindEvents() {
    $("btn-theme").addEventListener("click", toggleTheme);

    $("btn-reload-hub").addEventListener("click", function () {
      runGlobalAction("btn-reload-hub", "/ui/api/reload", "Config reload");
    });

    $("btn-restart-all").addEventListener("click", function () {
      runGlobalAction("btn-restart-all", "/ui/api/services/restart-all", "Restart all");
    });

    $("btn-start-all").addEventListener("click", function () {
      runGlobalAction("btn-start-all", "/ui/api/services/start-all", "Start all");
    });

    $("btn-logout").addEventListener("click", async function () {
      try {
        await apiPost("/ui/api/logout");
      } catch (_) {}
      window.location.replace("/ui/");
    });

    document.querySelectorAll(".nav-tab").forEach(function (tab) {
      tab.addEventListener("click", function () {
        switchView(tab.dataset.view);
      });
    });

    document.querySelectorAll("#service-filters .filter-pill").forEach(function (pill) {
      pill.addEventListener("click", function () {
        state.serviceFilter = pill.dataset.filter;
        document.querySelectorAll("#service-filters .filter-pill").forEach(function (p) {
          p.classList.toggle("is-active", p === pill);
        });
        renderServicesTable();
      });
    });

    $("service-search").addEventListener("input", function (ev) {
      state.serviceSearch = ev.target.value.trim();
      renderServicesTable();
    });

    $("btn-close-drawer").addEventListener("click", function () {
      selectService(null);
    });

    $("btn-copy-endpoint").addEventListener("click", function () {
      var val = $("drawer-endpoint").textContent;
      if (val && val !== "—") copyText(val, "endpoint URL");
    });

    $("btn-copy-snippet").addEventListener("click", function () {
      var txt = $("drawer-snippet").textContent;
      if (txt) copyText(txt, state.snippetClient + " config");
    });

    document.querySelectorAll("#snippet-tabs .snippet-tab").forEach(function (tab) {
      tab.addEventListener("click", function () {
        state.snippetClient = tab.dataset.client;
        document.querySelectorAll("#snippet-tabs .snippet-tab").forEach(function (t) {
          t.classList.toggle("is-active", t === tab);
        });
        if (state.selectedService) {
          loadSnippetForDrawer(state.selectedService, state.snippetClient);
        }
      });
    });

    /* Logs controls */
    $("log-target-select").addEventListener("change", function (ev) {
      state.logTarget = ev.target.value;
      if (state.view === "logs") {
        startLogStream(state.logTarget);
      }
    });

    $("log-filter-input").addEventListener("input", function (ev) {
      state.logFilter = ev.target.value.trim();
      renderLogs();
    });

    $("btn-log-pause").addEventListener("click", function () {
      state.logPaused = !state.logPaused;
      var btn = $("btn-log-pause");
      if (state.logPaused) {
        btn.textContent = "Resume";
        btn.classList.add("is-toggled");
        setLogStreamBadge("warn", "Paused");
      } else {
        btn.textContent = "Pause";
        btn.classList.remove("is-toggled");
        if (state.logPending.length > 0) {
          state.logEntries = state.logEntries.concat(state.logPending).slice(-MAX_LOG_BUFFER);
          state.logPending = [];
          renderLogs();
        }
        setLogStreamBadge("ok", "Streaming");
      }
    });

    $("btn-log-autoscroll").addEventListener("click", function () {
      state.logAutoScroll = !state.logAutoScroll;
      var btn = $("btn-log-autoscroll");
      btn.classList.toggle("is-toggled", state.logAutoScroll);
      btn.setAttribute("aria-pressed", state.logAutoScroll ? "true" : "false");
      if (state.logAutoScroll) {
        var vp = $("log-viewport");
        vp.scrollTop = vp.scrollHeight;
      }
    });

    $("btn-log-clear").addEventListener("click", function () {
      state.logEntries = [];
      state.logPending = [];
      renderLogs();
    });

    $("btn-log-copy").addEventListener("click", function () {
      var lines = state.logEntries.filter(matchesLogFilter).map(function (e) {
        return (e.timestamp || "") + " [" + (e.stream || "log") + "] " + (e.line || "");
      });
      if (lines.length === 0) {
        toast("No visible log lines to copy", true);
        return;
      }
      copyText(lines.join("\n"), lines.length + " log lines");
    });

    /* Tools & Analytics controls */
    $("analytics-search").addEventListener("input", function (ev) {
      state.analyticsSearch = ev.target.value.trim();
      renderToolAnalytics();
    });

    document.querySelectorAll(".analytics-table .sort-btn").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var key = btn.dataset.sort;
        if (state.analyticsSortKey === key) {
          state.analyticsSortDir = state.analyticsSortDir === "asc" ? "desc" : "asc";
        } else {
          state.analyticsSortKey = key;
          state.analyticsSortDir = (key === "service" || key === "tool") ? "asc" : "desc";
        }
        renderToolAnalytics();
      });
    });

    $("explorer-service-select").addEventListener("change", function (ev) {
      state.explorerService = ev.target.value;
    });

    $("btn-inspect-tools").addEventListener("click", inspectSelectedService);

    $("explorer-filter").addEventListener("input", function (ev) {
      state.explorerFilter = ev.target.value.trim();
      renderExplorerResult();
    });

    document.addEventListener("keydown", function (ev) {
      if (ev.key === "Escape" && state.selectedService) {
        selectService(null);
      }
    });

    window.addEventListener("beforeunload", function () {
      if (state.eventSource) state.eventSource.close();
      if (state.logSource) state.logSource.close();
    });
  }

  /* Bootstrap */
  function init() {
    initTheme();
    var csrfMeta = document.querySelector('meta[name="mcphub-csrf"]');
    if (csrfMeta) state.csrfToken = csrfMeta.getAttribute("content") || "";
    var authMeta = document.querySelector('meta[name="mcphub-auth"]');
    if (authMeta) state.authMode = authMeta.getAttribute("content") || "token";

    var logoutBtn = $("btn-logout");
    if (logoutBtn) logoutBtn.hidden = (state.authMode === "noauth");

    bindEvents();
    loadDashboardData();
    connectEventStream();

    window.setInterval(function () {
      if (document.visibilityState === "visible") {
        loadDashboardData();
      }
    }, 5000);
  }

  init();
})();
