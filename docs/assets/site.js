(function () {
  var NAV = [
    { pages: ["introduction"] },
    { title: "Getting Started", pages: ["installation", "quickstart", "concepts", "ollama", "troubleshooting"] },
    { title: "Foundations", pages: ["providers", "generating-text", "streaming", "tools", "structured-output", "threads", "human-in-the-loop"] },
    { title: "Agents", pages: ["agent-loop", "multi-agent", "evals", "lifecycles"] },
    { title: "Plugins", pages: ["plugins", "plugins-choose", "kind-tools", "kind-hooks", "kind-models", "kind-threads-memory", "kind-sandboxes", "kind-triggers", "kind-telemetry", "kind-secrets", "kind-policy", "plugin-permissions", "runtime-plugins", "plugin-packages", "plugin-api"] },
    { title: "Security", pages: ["security", "production-checklist"] },
    { title: "Reference", pages: ["reference-options", "reference-run", "reference-result", "cli"] }
  ];
  var ICON = {
    copy: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>',
    check: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" aria-hidden="true"><path d="m5 12 5 5 9-10"/></svg>',
    chev: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg>',
    file: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/></svg>',
    term: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="m5 8 4 4-4 4M12 17h7"/></svg>'
  };

  var pages = Object.create(null), order = [], groupOf = Object.create(null);
  document.querySelectorAll("section.page").forEach(function (s) { pages[s.id] = s; });
  NAV.forEach(function (g) { g.pages.forEach(function (id) { if (pages[id]) { order.push(id); groupOf[id] = g.title || ""; } }); });
  function title(id) { var p = pages[id]; if (p.dataset.nav) return p.dataset.nav; var h = p.querySelector("h1"); return h ? h.textContent.trim() : id; }

  // ---- sidebar ----
  var sidenav = document.getElementById("sidenav");
  var openGroups = {};
  try { openGroups = JSON.parse(localStorage.getItem("eip-nav-open") || "{}") || {}; } catch (e) {}
  function saveGroups() { try { localStorage.setItem("eip-nav-open", JSON.stringify(openGroups)); } catch (e) {} }
  NAV.forEach(function (g) {
    if (!g.title) {
      g.pages.forEach(function (id) { sidenav.insertAdjacentHTML("beforeend", '<a class="item top-item" href="#' + id + '">' + title(id) + "</a>"); });
      return;
    }
    var open = openGroups[g.title] !== undefined ? !!openGroups[g.title] : g.title === "Getting Started";
    var html = '<div class="group" data-open="' + open + '"><button type="button" aria-expanded="' + open + '">' + g.title + ICON.chev + '</button><div class="items">';
    g.pages.forEach(function (id) { if (pages[id]) html += '<a class="item" href="#' + id + '">' + title(id) + "</a>"; });
    sidenav.insertAdjacentHTML("beforeend", html + "</div></div>");
  });
  sidenav.querySelectorAll(".group > button").forEach(function (b) {
    b.addEventListener("click", function () {
      var g = b.parentNode, open = g.dataset.open !== "true";
      g.dataset.open = String(open); b.setAttribute("aria-expanded", String(open));
      openGroups[g.querySelector("button").textContent.trim()] = open; saveGroups();
    });
  });

  // ---- page chrome: title row with Copy page ----
  order.forEach(function (id) {
    var h1 = pages[id].querySelector("h1");
    if (!h1) return;
    var row = document.createElement("div");
    row.className = "titlerow";
    h1.parentNode.insertBefore(row, h1);
    row.appendChild(h1);
    var b = document.createElement("button");
    b.type = "button"; b.className = "copypage";
    b.innerHTML = ICON.copy + "<span>Copy page</span>";
    b.addEventListener("click", function () {
      var clone = pages[id].cloneNode(true);
      clone.querySelectorAll(".anchor,.copypage,.toc-mobile,.pager,.site-footer,.lab,.hero-art,.hero-actions,.demo-frame,.copy").forEach(function (x) { x.remove(); });
      copyText(clone.innerText.replace(/\n{3,}/g, "\n\n"), b, "<span>Copied</span>", "<span>Copy page</span>");
    });
    row.appendChild(b);
  });

  // ---- code blocks: file bar, line numbers, highlighting, copy ----
  document.querySelectorAll(".code").forEach(function (box) {
    var pre = box.querySelector("pre"), code = box.querySelector("code");
    var title = box.dataset.title || "";
    var isTerm = title === "terminal";
    if (isTerm) box.classList.add("plain");
    try { if (window.hljs && code.className.indexOf("language-") === 0) hljs.highlightElement(code); } catch (e) {}
    var lines = code.textContent.replace(/\n$/, "").split("\n").length;
    var bar = document.createElement("div");
    bar.className = "bar";
    bar.innerHTML = '<span class="file">' + (isTerm ? ICON.term : ICON.file) + "<span></span></span>";
    bar.querySelector(".file span").textContent = title;
    var btn = document.createElement("button");
    btn.type = "button"; btn.className = "copy"; btn.setAttribute("aria-label", "Copy code"); btn.innerHTML = ICON.copy;
    btn.addEventListener("click", function () { copyText(code.innerText, btn, ICON.check, ICON.copy); });
    bar.appendChild(btn);
    var body = document.createElement("div");
    body.className = "body";
    var nums = document.createElement("div");
    nums.className = "nums"; nums.setAttribute("aria-hidden", "true");
    var n = []; for (var i = 1; i <= lines; i++) n.push(i);
    nums.textContent = n.join("\n");
    box.insertBefore(bar, pre);
    box.insertBefore(body, pre);
    body.appendChild(nums); body.appendChild(pre);
  });

  // ---- tabs ----
  document.querySelectorAll("[data-tabs]").forEach(function (group) {
    var list = group.querySelector('[role="tablist"]');
    var tabs = group.querySelectorAll('[role="tab"]'), panels = group.querySelectorAll('[role="tabpanel"]');
    var cp = document.createElement("button");
    cp.type = "button"; cp.className = "copy tabcopy"; cp.setAttribute("aria-label", "Copy code"); cp.innerHTML = ICON.copy;
    cp.addEventListener("click", function () {
      var vis = Array.prototype.filter.call(panels, function (p) { return !p.hidden; })[0];
      if (vis) copyText(vis.querySelector("code").innerText, cp, ICON.check, ICON.copy);
    });
    list.appendChild(cp);
    tabs.forEach(function (tab, i) {
      tab.addEventListener("click", function () {
        tabs.forEach(function (t, j) { t.setAttribute("aria-selected", String(i === j)); panels[j].hidden = i !== j; });
      });
    });
  });

  function copyText(text, btn, done, idle) {
    var ok = function () { btn.innerHTML = btn.innerHTML.replace(idle, done); setTimeout(function () { btn.innerHTML = btn.innerHTML.replace(done, idle); }, 1400); };
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(ok, function () {});
        return;
      }
    } catch (e) {}
    try {
      var ta = document.createElement("textarea");
      ta.value = text; ta.setAttribute("readonly", ""); ta.style.position = "fixed"; ta.style.opacity = "0";
      document.body.appendChild(ta); ta.select(); document.execCommand("copy"); document.body.removeChild(ta); ok();
    } catch (e) {}
  }

  // ---- routing: one page at a time ----
  var crumbs = document.getElementById("crumbs"), pager = document.getElementById("pager"), toc = document.getElementById("toc");
  var current = null, labController = null;
  // Pages merged into the Plugins section in v0.4; old links keep working.
  var MOVED = Object.assign(Object.create(null), { "writing-plugins": "plugins", "middleware": "kind-hooks", "sandboxes": "kind-sandboxes", "persistence": "kind-threads-memory",
    "integrations": "kind-tools", "automation": "kind-triggers", "observability": "kind-telemetry", "secrets": "kind-secrets",
    "policy": "kind-policy", "reference-hooks": "plugin-api", "reference-plugins": "plugins-choose" });
  function pageFor(hash) {
    if (MOVED[hash]) hash = MOVED[hash];
    if (pages[hash]) return { page: hash };
    var el = hash && document.getElementById(hash);
    if (el) { var s = el.closest("section.page"); if (s) return { page: s.id, anchor: el }; }
    return { page: "introduction" };
  }
  function show(hash, fromPop) {
    var t = pageFor(hash), id = t.page, was = current;
    if (current !== id) {
      if (labController) labController.stop();
      document.querySelectorAll("section.page").forEach(function (p) { p.hidden = p.id !== id; });
      current = id;
      document.querySelectorAll(".side a.item").forEach(function (a) {
        var on = a.getAttribute("href") === "#" + id;
        if (on) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
        if (on) { var g = a.closest(".group"); if (g) { g.dataset.open = "true"; g.querySelector("button").setAttribute("aria-expanded", "true"); } }
      });
      var grp = groupOf[id];
      crumbs.innerHTML = grp ? "<span>" + grp + "</span>" + ICON.chev + "<span>" + title(id) + "</span>" : "<span>Docs</span>";
      var i = order.indexOf(id), prev = order[i - 1], next = order[i + 1];
      pager.innerHTML = (prev ? '<a href="#' + prev + '"><small>Previous</small><b>' + title(prev) + "</b></a>" : "<span></span>") +
                        (next ? '<a class="next" href="#' + next + '"><small>Next</small><b>' + title(next) + "</b></a>" : "");
      var hs = id === "introduction" ? pages[id].querySelectorAll(".section-head h2") : pages[id].querySelectorAll("h2, h3");
      toc.innerHTML = "";
      hs.forEach(function (h, k) {
        if (!h.id) h.id = id + "-" + (k + 1);
        var target = h.id;
        var sec = h.closest(".section");
        if (sec && sec.id) target = sec.id;
        var label = h.cloneNode(true);
        label.querySelectorAll(".anchor").forEach(function (x) { x.remove(); });
        var a = document.createElement("a"); a.href = "#" + target; a.textContent = h.getAttribute("data-toc") || label.innerText.trim(); toc.appendChild(a);
      });
      if (!hs.length) toc.innerHTML = '<a href="#' + id + '" class="on">' + title(id) + "</a>";
      var oldMobile = pages[id].querySelector(".toc-mobile");
      if (oldMobile) oldMobile.remove();
      if (hs.length) {
        var det = document.createElement("details");
        det.className = "toc-mobile";
        var sum = document.createElement("summary");
        sum.textContent = "On this page";
        det.appendChild(sum);
        hs.forEach(function (h) {
          var target = h.id;
          var sec = h.closest(".section");
          if (sec && sec.id) target = sec.id;
          var label = h.cloneNode(true);
          label.querySelectorAll(".anchor").forEach(function (x) { x.remove(); });
          var a = document.createElement("a");
          a.href = "#" + target;
          a.textContent = h.getAttribute("data-toc") || label.innerText.trim();
          det.appendChild(a);
        });
        var anchor = pages[id].querySelector(".titlerow") || pages[id].querySelector("h1");
        if (anchor) anchor.parentNode.insertBefore(det, anchor.nextSibling);
      }
      var top = hash === "plugin-lab" ? "lab" : id.indexOf("reference") === 0 || id === "cli" ? "reference" : id === "providers" ? "providers" : "docs";
      document.querySelectorAll(".toplinks a").forEach(function (a) { if (a.dataset.top === top) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current"); });
      try { document.title = title(id) + " · eipagent Docs"; } catch (e) {}
      document.body.classList.remove("nav-open");
      document.getElementById("menubtn").setAttribute("aria-expanded", "false");
      document.body.setAttribute("data-page", id);
      var mainEl = document.getElementById("main");
      if (mainEl && !reduceMotion) { mainEl.classList.remove("page-in"); void mainEl.offsetWidth; mainEl.classList.add("page-in"); }
      if (id === "introduction" && was && was !== "introduction" && labController) labController.start();
    }
    if (t.anchor) t.anchor.scrollIntoView(); else window.scrollTo(0, 0);
    spy();
  }
  document.addEventListener("click", function (e) {
    var a = e.target.closest && e.target.closest('a[href^="#"]');
    if (!a) return;
    var h = a.getAttribute("href").slice(1);
    if (!h) return;
    e.preventDefault();
    try { history.pushState(null, "", "#" + h); } catch (err) {}
    show(h);
    closePalette();
  });
  window.addEventListener("hashchange", function () { show(location.hash.slice(1), true); });
  window.addEventListener("popstate", function () { show(location.hash.slice(1), true); });

  // ---- on this page: scrollspy ----
  function spy() {
    var links = toc.querySelectorAll("a"), on = null;
    links.forEach(function (a) {
      var el = document.getElementById(a.getAttribute("href").slice(1));
      if (el && el.getBoundingClientRect().top < 140) on = a;
    });
    links.forEach(function (a) { a.classList.toggle("on", a === on || (!on && a === links[0] && links.length === 1)); });
  }
  window.addEventListener("scroll", spy, { passive: true });
  document.getElementById("totop").addEventListener("click", function () { window.scrollTo({ top: 0, behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" }); });

  // ---- mobile nav: a drawer that closes on a tap outside it, a link, or Escape ----
  var mb = document.getElementById("menubtn"), side = document.getElementById("side");
  function setNav(open) {
    document.body.classList.toggle("nav-open", open);
    mb.setAttribute("aria-expanded", String(open));
    mb.setAttribute("aria-label", open ? "Close navigation" : "Open navigation");
  }
  mb.addEventListener("click", function (e) { e.stopPropagation(); setNav(!document.body.classList.contains("nav-open")); });
  document.getElementById("scrim").addEventListener("click", function () { setNav(false); });
  document.addEventListener("click", function (e) {
    if (!document.body.classList.contains("nav-open")) return;
    if (!side.contains(e.target) || (e.target.closest && e.target.closest("a.item"))) setNav(false);
  });
  document.addEventListener("keydown", function (e) { if (e.key === "Escape" && document.body.classList.contains("nav-open")) { setNav(false); mb.focus(); } });
  window.addEventListener("resize", function () { if (window.innerWidth > 900 && document.body.classList.contains("nav-open")) setNav(false); });

  // ---- tables: label each cell with its column, for the stacked layout on phones ----
  document.querySelectorAll(".table table").forEach(function (t) {
    var heads = Array.prototype.map.call(t.querySelectorAll("thead th"), function (th) { return th.textContent.trim(); });
    t.querySelectorAll("tbody tr").forEach(function (tr) {
      Array.prototype.forEach.call(tr.children, function (td, i) { if (heads[i]) td.setAttribute("data-label", heads[i]); });
    });
  });

  // ---- theme toggle ----
  var root = document.documentElement;
  try { var saved = localStorage.getItem("eip-theme"); if (saved) root.dataset.theme = saved; } catch (e) {}
  var themeBtn = document.getElementById("themebtn");
  var sunIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>';
  var moonIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/></svg>';
  function themeIsDark() { return root.dataset.theme ? root.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches; }
  function syncTheme() {
    var dark = themeIsDark();
    themeBtn.innerHTML = dark ? sunIcon : moonIcon;
    themeBtn.setAttribute("aria-pressed", String(dark));
    themeBtn.setAttribute("aria-label", dark ? "Switch to light mode" : "Switch to dark mode");
    document.querySelectorAll('meta[name="theme-color"]').forEach(function (m) { m.setAttribute("content", dark ? "#0e0e0d" : "#f6f4ee"); });
  }
  syncTheme();
  themeBtn.addEventListener("click", function () {
    root.dataset.theme = themeIsDark() ? "light" : "dark";
    try { localStorage.setItem("eip-theme", root.dataset.theme); } catch (e) {}
    syncTheme();
  });

  // ---- command palette ----
  var pal = document.getElementById("palette"), pq = document.getElementById("pq"), plist = document.getElementById("plist"), sel = 0, results = [];
  var index = [];
  order.forEach(function (id) {
    index.push({ href: id, label: title(id), where: groupOf[id] || "Docs", text: pages[id].textContent.toLowerCase() });
    pages[id].querySelectorAll("h2, h3").forEach(function (h, k) {
      if (!h.id) h.id = id + "-" + (k + 1);
      var label = h.getAttribute("data-toc") || h.innerText.replace(/\s+/g, " ").trim();
      index.push({ href: h.id, label: label, where: title(id), text: h.textContent.toLowerCase() });
    });
  });
  function render() {
    var q = pq.value.trim().toLowerCase();
    results = index.filter(function (r) { return !q || r.label.toLowerCase().indexOf(q) >= 0 || r.text.indexOf(q) >= 0; })
      .sort(function (a, b) { return (b.label.toLowerCase().indexOf(q) >= 0) - (a.label.toLowerCase().indexOf(q) >= 0); }).slice(0, 40);
    sel = Math.min(sel, Math.max(results.length - 1, 0));
    plist.innerHTML = results.length ? results.map(function (r, i) {
      return '<li><a href="#' + r.href + '" role="option" aria-selected="' + (i === sel) + '"><span>' + r.label + "</span><small>" + r.where + "</small></a></li>";
    }).join("") : '<li class="none">No results. Try another word.</li>';
  }
  var lastFocus = null;
  if (!pal.querySelector(".hint")) {
    var hint = document.createElement("div");
    hint.className = "hint";
    hint.innerHTML = "<span><kbd>↑</kbd><kbd>↓</kbd> navigate</span><span><kbd>↵</kbd> open</span><span><kbd>esc</kbd> close</span>";
    pal.querySelector(".box").appendChild(hint);
  }
  function openPalette() { lastFocus = document.activeElement; pal.hidden = false; document.body.style.overflow = "hidden"; pq.value = ""; sel = 0; render(); setTimeout(function () { pq.focus(); }, 0); }
  function closePalette() { if (pal.hidden) return; pal.hidden = true; document.body.style.overflow = ""; if (lastFocus && lastFocus.focus) { try { lastFocus.focus(); } catch (e) {} } }
  document.getElementById("searchbtn").addEventListener("click", openPalette);
  pq.addEventListener("input", function () { sel = 0; render(); });
  pal.addEventListener("click", function (e) { if (e.target === pal) closePalette(); });
  document.addEventListener("keydown", function (e) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") { e.preventDefault(); pal.hidden ? openPalette() : closePalette(); return; }
    if (e.key === "/" && pal.hidden && !/input|textarea/i.test(document.activeElement.tagName)) { e.preventDefault(); openPalette(); return; }
    if (pal.hidden) return;
    if (e.key === "Escape") closePalette();
    else if (e.key === "Tab") {
      var focusables = Array.prototype.slice.call(pal.querySelectorAll('input,button,a[href]')).filter(function (x) { return !x.disabled; });
      if (!focusables.length) return;
      var first = focusables[0], last = focusables[focusables.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }
    else if (e.key === "ArrowDown") { e.preventDefault(); sel = Math.min(sel + 1, results.length - 1); render(); }
    else if (e.key === "ArrowUp") { e.preventDefault(); sel = Math.max(sel - 1, 0); render(); }
    else if (e.key === "Enter" && results[sel]) { e.preventDefault(); try { history.pushState(null, "", "#" + results[sel].href); } catch (err) {} show(results[sel].href); closePalette(); }
  });


  // ---- reading experience ----
  var reduceMotion = matchMedia("(prefers-reduced-motion: reduce)").matches;

  order.forEach(function (id) {
    pages[id].querySelectorAll("h2, h3").forEach(function (h, k) {
      if (!h.id) h.id = id + "-" + (k + 1);
      if (id === "introduction" || h.closest(".section, .hero, .cta-band, .lab, .bento-card, .site-footer")) return;
      if (h.querySelector(".anchor")) return;
      var a = document.createElement("a");
      a.className = "anchor";
      a.href = "#" + h.id;
      a.setAttribute("aria-label", "Link to " + h.textContent.trim());
      a.textContent = "#";
      h.appendChild(a);
    });
  });

  var readbar = document.createElement("div");
  readbar.className = "readbar";
  document.body.appendChild(readbar);
  var readTick = false;
  function updateReadbar() {
    readTick = false;
    var d = document.documentElement;
    var max = d.scrollHeight - d.clientHeight;
    readbar.style.width = (max > 0 ? (d.scrollTop / max) * 100 : 0) + "%";
  }
  window.addEventListener("scroll", function () {
    if (!readTick) { readTick = true; requestAnimationFrame(updateReadbar); }
  }, { passive: true });

  var revealEls = Array.prototype.slice.call(document.querySelectorAll(".reveal"));
  if (reduceMotion || !("IntersectionObserver" in window)) {
    revealEls.forEach(function (el) { el.classList.add("in"); });
  } else {
    var revealObs = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) {
        if (en.isIntersecting) { en.target.classList.add("in"); revealObs.unobserve(en.target); }
      });
    }, { rootMargin: "0px 0px -8% 0px", threshold: .08 });
    revealEls.forEach(function (el) { revealObs.observe(el); });
  }

  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("[data-copy]");
    if (!b) return;
    e.preventDefault();
    copyText(b.getAttribute("data-copy"), b, ICON.check, ICON.copy);
  });

  // ---- plugin lab ----
  var LAB_PLUGINS = [
    { id: "tool_shell", group: "Sandbox tools", plane: "sandbox", name: "tool_shell", desc: "Runs bash inside the sandbox.", tags: ["sandbox"], changes: ["adds the bash tool inside the sandbox", "tests and git commands run next to the agent"], summary: ["+ bash"], config: ['  eipagent.WithPlugins("tool_shell"),'] },
    { id: "tool_fs", group: "Sandbox tools", plane: "sandbox", name: "tool_fs", desc: "Reads and writes files inside the sandbox.", tags: ["sandbox"], changes: ["adds read_file, write_file, edit_file, list_dir, find_files and grep inside the sandbox"], summary: ["+ file tools"], config: ['  eipagent.WithPlugins("tool_fs"),'] },
    { id: "tool_fetch", group: "Sandbox tools", plane: "sandbox", name: "tool_fetch", desc: "Fetches allowed URLs through the host egress broker.", tags: ["sandbox", "net"], changes: ["adds http_fetch inside the sandbox", "every request goes through the host egress broker"], summary: ["+ fetch"], config: ['  eipagent.WithPlugins("tool_fetch"),'] },
    { id: "hook_approval", group: "Safety", plane: "sandbox", name: "hook_approval", desc: "Pauses before the pull-request call.", tags: ["sandbox", "+ approval"], changes: ["pauses before http_fetch POST /pulls until a person decides"], summary: ["+ approval"], config: ['  eipagent.WithPlugin("hook_approval", map[string]any{"interrupt_on": map[string]any{"http_fetch": true}}),'] },
    { id: "secrets_vault", group: "Safety", plane: "host", host: true, name: "secrets_vault", desc: "Keeps GITHUB_TOKEN on the host; the sandbox gets a placeholder.", tags: ["host", "+ secret"], changes: ["keeps GITHUB_TOKEN on the host", "the sandbox only sees an eipsec_ placeholder"], summary: ["+ host secret"] },
    { id: "audit_file", group: "Operations", plane: "host", host: true, name: "audit_file", desc: "Writes host-side audit records, including sandbox.egress.", tags: ["host", "+ audit"], changes: ["writes host audit records for model, tool and egress events"], summary: ["+ host audit"] },
    { id: "hook_compact", group: "Operations", plane: "sandbox", name: "hook_compact", desc: "Compacts long test logs inside the sandbox.", tags: ["sandbox", "+ context"], changes: ["compacts 12,480 chars of test output when context passes 80%"], summary: ["+ context guard"], config: ['  eipagent.WithPlugin("hook_compact", map[string]any{"max_fraction": 0.8}),'] },
    { id: "trace_otlp", group: "Operations", plane: "host", host: true, name: "trace_otlp", desc: "Exports host and sandbox spans to OpenTelemetry.", tags: ["host", "+ traces"], changes: ["exports one trace per run, with host and sandbox spans"], summary: ["+ traces"] }
  ];
  var LAB_BY = {};
  LAB_PLUGINS.forEach(function (p) { LAB_BY[p.id] = p; });
  var LAB_PRESETS = {
    prototype: ["tool_shell", "tool_fs"],
    team: ["tool_shell", "tool_fs", "tool_fetch", "secrets_vault", "audit_file", "hook_approval"],
    production: ["tool_shell", "tool_fs", "tool_fetch", "hook_approval", "secrets_vault", "audit_file", "hook_compact", "trace_otlp"]
  };

  function makeSel(ids) { var o = {}; (ids || []).forEach(function (id) { o[id] = true; }); return o; }
  function selectedIds(sel) { return Object.keys(sel).filter(function (id) { return sel[id] && LAB_BY[id]; }); }
  function esc(s) { return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"); }

  function labConfig(sel) {
    var ids = selectedIds(sel);
    var host = ids.filter(function (id) { return LAB_BY[id].host; });
    var sandbox = ids.filter(function (id) { return !LAB_BY[id].host; });
    var toolIds = sandbox.filter(function (id) { return id.indexOf("tool_") === 0; });
    var otherIds = sandbox.filter(function (id) { return id.indexOf("tool_") !== 0; });
    var lines = [
      "agent, err := eipagent.New(",
      '  eipagent.WithModel("anthropic:claude-sonnet-5-5"),',
      '  eipagent.WithInstructions("You are a careful coding agent."),',
      "",
      "  // The agent, its plugins and its tools run inside this cloud sandbox.",
      "  eipagent.WithRunIn(eipagent.RunIn{",
      '    Provider: "tensorlake",',
      '    Spec:     sdk.SandboxSpec{Image: "tensorlake/ubuntu-minimal", Network: sdk.NetworkPolicy{Mode: "none"}},'
    ];
    if (host.length) {
      lines.push("    HostPlugins: []eipagent.PluginSpec{");
      host.forEach(function (id) {
        if (id === "audit_file") lines.push('      {Use: "audit_file", Config: map[string]any{"path": "audit.jsonl"}},');
        else if (id === "trace_otlp") lines.push('      {Use: "trace_otlp", Config: map[string]any{"endpoint": "http://otel-collector:4318"}},');
        else lines.push('      {Use: "' + id + '"},');
      });
      lines.push("    },");
    }
    if (sel.secrets_vault) lines.push('    SecretHosts: map[string][]string{"GITHUB_TOKEN": []string{"api.github.com"}},');
    lines.push("  }),");
    if (sandbox.length) {
      lines.push("");
      lines.push("  // These load inside the sandbox.");
      if (toolIds.length) lines.push('  eipagent.WithPlugins(' + toolIds.map(function (id) { return '"' + id + '"'; }).join(", ") + "),");
      otherIds.forEach(function (id) { LAB_BY[id].config.forEach(function (line) { lines.push(line); }); });
    }
    lines.push(")");
    return lines.join("\n");
  }

  function labTrace(sel) {
    var ev = [];
    function add(plane, type, title, detail, status, plugin) {
      ev.push({ plane: plane, type: type, title: title, detail: detail, status: status || "ok", plugin: plugin || "" });
    }
    add("host", "run", "run.start", "host process · builds the RunIn spec", "ok");
    add("cloud", "cloud", "sandbox.create", "tensorlake/ubuntu-minimal · x86_64 Linux · network none", "ok");
    add("sandbox", "agent", "agent.start", "eip serve --stdio · no secret env vars", "ok");
    if (sel.tool_shell) add("sandbox", "tool", "tool.call bash", "go test ./... · 12,480 chars of output", "ok", "tool_shell");
    if (sel.tool_fs) add("sandbox", "tool", "tool.call read_file", "logs/test.log · 41 lines", "ok", "tool_fs");
    add("host", "secret", "host.model", "model key stays on the host · sandbox holds none", "ok");
    add("host", "model", "model.call", "host calls the model with its key · decides to open a PR", "acc");
    if (sel.hook_compact) add("sandbox", "context", "hook_compact · context_usage", "83% → old tool output cleared → 61%", "ok", "hook_compact");
    if (sel.secrets_vault) add("boundary", "secret", "host.secret", "plugin asks for GITHUB_TOKEN → eipsec_… placeholder", "ok", "secrets_vault");
    if (sel.hook_approval && sel.tool_fetch) {
      add("sandbox", "wait", "hook_approval · interrupt", "about to POST /repos/acme/app/pulls", "wait", "hook_approval");
      add("sandbox", "decision", "decision", "pending · Ada is deciding", "wait", "hook_approval");
    }
    if (sel.tool_fetch) {
      add("sandbox", "tool", "tool.call http_fetch", "POST /repos/acme/app/pulls · with placeholder", "ok", "tool_fetch");
      if (sel.secrets_vault) add("boundary", "egress", "sandbox.egress", "allowed · api.github.com · host swapped GITHUB_TOKEN", "ok", "secrets_vault");
      else add("boundary", "egress", "sandbox.egress", "denied · 401 · no GITHUB_TOKEN grant", "bad", "tool_fetch");
    } else {
      add("sandbox", "model", "model.call", "http_fetch is not loaded · no PR can be opened", "acc");
    }
    add("sandbox", "model", "model.call", (sel.tool_fetch && sel.secrets_vault) ? "final answer · PR opened" : "final answer · explains what is missing", "acc");
    if (sel.audit_file) add("host", "audit", "audit_file · write", "host records model, tool and sandbox.egress", "ok", "audit_file");
    if (sel.trace_otlp) add("host", "trace", "trace_otlp · export", "one trace · host and sandbox spans", "ok", "trace_otlp");
    add("host", "cloud", "sandbox.destroy", "ephemeral sandbox deleted", "ok");
    add("host", "run", "run.end", "stop · sandbox gone", "ok");
    return ev;
  }

  (function initLab() {
    var lab = document.querySelector("[data-lab]");
    if (!lab) return;
    var rowsEl = lab.querySelector("[data-lab-plugins]");
    var countEl = lab.querySelector("[data-lab-count]");
    var statusEl = lab.querySelector("[data-lab-status]");
    var summaryEl = lab.querySelector("[data-lab-summary]");
    var configEl = lab.querySelector("[data-lab-config]");
    var diffEl = lab.querySelector("[data-lab-diff]");
    var traceEl = lab.querySelector("[data-lab-trace]");
    var liveEl = lab.querySelector("[data-lab-live]");
    var livebar = lab.querySelector("[data-lab-livebar]");
    var progressEl = lab.querySelector("[data-lab-progress]");
    var runBtn = lab.querySelector("[data-lab-run]");
    var stepBtn = lab.querySelector("[data-lab-step]");
    var resetBtn = lab.querySelector("[data-lab-reset]");
    var modeBtns = lab.querySelectorAll("[data-lab-mode]");
    var tabBtns = lab.querySelectorAll("[data-lab-tab]");
    var viewBtns = lab.querySelectorAll("[data-lab-view]");
    var viewPanels = lab.querySelectorAll("[data-lab-view-panel]");
    var labView = "run";
    var panes = lab.querySelectorAll("[data-lab-pane]");

    var sel = makeSel(LAB_PRESETS.team);
    var playMode = "auto";
    var stepIndex = 0;
    var STEP_MS = 900;
    var lastSel = null;
    var events = [];
    var timers = [];
    var pendingIndex = -1;
    var stopped = false;
    var started = false;

    var groups = [];
    LAB_PLUGINS.forEach(function (p) { if (groups.indexOf(p.group) < 0) groups.push(p.group); });
    var rows = "";
    groups.forEach(function (g) {
      rows += '<div class="lab-group"><h5>' + esc(g) + "</h5>";
      LAB_PLUGINS.filter(function (p) { return p.group === g; }).forEach(function (p) {
        rows += '<button type="button" class="lab-plugin" role="switch" aria-checked="false" data-plugin="' + p.id + '">' +
          "<span><b>" + esc(p.name) + "</b><small>" + esc(p.desc) + '</small><span class="chips">' +
          p.tags.map(function (t) { return '<span class="chip">' + esc(t) + "</span>"; }).join("") +
          '</span></span><span class="lab-switch" aria-hidden="true"></span></button>';
      });
      rows += "</div>";
    });
    rowsEl.innerHTML = rows;

    function stopTimers() {
      timers.forEach(function (t) { clearTimeout(t); });
      timers = [];
      stopped = true;
    }

    function setStatus(text, state) {
      if (statusEl) statusEl.textContent = text;
      if (livebar && state) livebar.dataset.state = state;
    }
    function setLive(text, state) {
      if (liveEl) liveEl.textContent = text;
      if (livebar && state) livebar.dataset.state = state;
    }
    function setProgress(pct) {
      if (progressEl) progressEl.style.width = Math.max(0, Math.min(100, pct)) + "%";
    }

    function renderRows() {
      rowsEl.querySelectorAll("[data-plugin]").forEach(function (b) {
        var on = !!sel[b.getAttribute("data-plugin")];
        b.setAttribute("aria-checked", String(on));
      });
      var n = selectedIds(sel).length;
      if (countEl) countEl.textContent = n + " selected";
    }

    function renderSummary() {
      var ids = selectedIds(sel);
      if (!ids.length) { summaryEl.innerHTML = '<span>no optional plugins</span>'; return; }
      var out = ["<span>" + ids.length + " plugins</span>"];
      ids.forEach(function (id) {
        LAB_BY[id].summary.forEach(function (s) {
          out.push('<span class="' + (s.charAt(0) === "+" ? "add" : "") + '">' + esc(s) + "</span>");
        });
      });
      summaryEl.innerHTML = out.join("");
    }

    function renderConfig() {
      configEl.innerHTML = '<div class="code plain"><pre><code class="language-go"></code></pre></div>';
      var code = configEl.querySelector("code");
      code.textContent = labConfig(sel);
      try { if (window.hljs) hljs.highlightElement(code); } catch (e) {}
    }

    function renderDiff() {
      var out = [];
      selectedIds(sel).forEach(function (id) {
        LAB_BY[id].changes.forEach(function (c) { out.push({ k: "add", t: c }); });
      });
      Object.keys(lastSel || {}).forEach(function (id) {
        if (!sel[id] && LAB_BY[id]) out.push({ k: "rm", t: "removes " + LAB_BY[id].name });
      });
      if (!out.length) {
        diffEl.innerHTML = '<h5>What changed</h5><p style="margin:0;color:var(--fg-3);font-size:13px">Nothing yet. Toggle a plugin.</p>';
      } else {
        diffEl.innerHTML = '<h5>What changed</h5><ul class="lab-diff-list">' + out.map(function (o) {
          return '<li class="' + o.k + '">' + esc(o.t) + "</li>";
        }).join("") + "</ul>";
      }
    }

    function matchesPreset(ids) {
      var key = ids.slice().sort().join(",");
      for (var name in LAB_PRESETS) {
        if (LAB_PRESETS[name].slice().sort().join(",") === key) return name;
      }
      return "";
    }

    function updatePresets() {
      var active = matchesPreset(selectedIds(sel));
      lab.querySelectorAll("[data-preset]").forEach(function (b) {
        b.setAttribute("aria-pressed", String(b.getAttribute("data-preset") === active));
      });
    }

    function setPlane(plane) {
      lab.querySelectorAll("[data-plane-node]").forEach(function (n) {
        var v = n.getAttribute("data-plane-node");
        var on = v === plane || (plane === "sandbox" && v === "cloud") || (plane === "boundary" && (v === "host" || v === "cloud"));
        n.classList.toggle("active", !!on);
      });
    }

    function traceHTML() {
      return events.map(function (e, i) {
        return '<div class="trace-event ' + e.status + '" data-i="' + i + '" tabindex="0" role="button" aria-expanded="false">' +
          '<span class="trace-dot"></span><div class="line"><div class="type"><span>' + esc(e.title) + "</span>" +
          (e.plane ? '<span class="plane ' + esc(e.plane) + '">' + esc(e.plane) + "</span>" : "") +
          (e.plugin ? '<span class="plugin">' + esc(e.plugin) + "</span>" : "") +
          '</div><p class="detail">' + esc(e.detail) + '</p><p class="meta">' + esc(e.type) + " · +" + (i * 0.15).toFixed(2) + 's</p>' +
          '<div class="trace-extra"><dl><dt>Plane</dt><dd>' + esc(e.plane) + '</dd><dt>Plugin</dt><dd>' + esc(e.plugin || "core") + '</dd><dt>Step</dt><dd>' + esc(e.type) + " · " + esc(e.title) + '</dd></dl></div>' +
          (e.type === "decision" ? '<div class="lab-approval-slot"></div>' : "") +
          "</div></div>";
      }).join("");
    }

    traceEl.addEventListener("click", function (e) {
      var ev = e.target.closest(".trace-event");
      if (!ev || e.target.closest(".lab-approval")) return;
      var open = ev.classList.toggle("open");
      ev.setAttribute("aria-expanded", String(open));
    });
    traceEl.addEventListener("keydown", function (e) {
      var ev = e.target.closest(".trace-event");
      if (!ev || (e.key !== "Enter" && e.key !== " ")) return;
      e.preventDefault();
      var open = ev.classList.toggle("open");
      ev.setAttribute("aria-expanded", String(open));
    });

    function showApproval(i) {
      var el = traceEl.querySelector('[data-i="' + i + '"] .lab-approval-slot');
      if (!el) return;
      el.innerHTML = '<div class="lab-approval"><b>Waiting on Ada</b><p>About to POST /repos/acme/app/pulls with GITHUB_TOKEN. Approve the pull request?</p><div class="actions"><button type="button" class="ok" data-approve>Approve</button><button type="button" data-reject>Reject</button></div></div>';
    }

    function scrollTraceTo(i) {
      var el = traceEl.querySelector('[data-i="' + i + '"]');
      if (!el) return;
      var er = el.getBoundingClientRect(), tr = traceEl.getBoundingClientRect();
      if (er.bottom > tr.bottom - 10) {
        traceEl.scrollTo({ top: traceEl.scrollTop + (er.bottom - tr.bottom) + 18, behavior: reduceMotion ? "auto" : "smooth" });
      } else if (er.top < tr.top + 10) {
        traceEl.scrollTo({ top: Math.max(0, traceEl.scrollTop - (tr.top - er.top) - 18), behavior: reduceMotion ? "auto" : "smooth" });
      }
    }

    function nextStep() {
      if (playMode !== "step" || pendingIndex >= 0) return;
      if (stepIndex >= events.length) {
        setStatus("Complete", "done");
        setLive("Complete · " + events.length + " events", "done");
        setPlane("");
        setProgress(100);
        return;
      }
      var i = stepIndex;
      var el = traceEl.querySelector('[data-i="' + i + '"]');
      if (el) el.classList.add("in");
      scrollTraceTo(i);
      setPlane(events[i].plane);
      setLive(events[i].plane.toUpperCase() + " · " + events[i].title, "running");
      setProgress(((i + 1) / events.length) * 100);
      stepIndex++;
      if (events[i].type === "decision") {
        pendingIndex = i;
        if (stepBtn) stepBtn.disabled = true;
        setStatus("Waiting for approval", "waiting");
        setLive("WAITING · Ada approval", "waiting");
        showApproval(i);
        return;
      }
      if (stepIndex >= events.length) {
        setStatus("Complete", "done");
        setLive("Complete · " + events.length + " events", "done");
        setPlane("");
      } else {
        setStatus("Step " + stepIndex + "/" + events.length, "running");
      }
    }

    function play(from) {
      stopTimers();
      stopped = false;
      pendingIndex = -1;
      var i = from || 0;
      if (!i) traceEl.scrollTop = 0;
      if (reduceMotion) {
        traceEl.querySelectorAll(".trace-event").forEach(function (el) { el.classList.add("in"); });
        var di = -1;
        events.forEach(function (e, n) { if (e.type === "decision") di = n; });
        if (di >= 0) { pendingIndex = di; setStatus("Waiting for approval", "waiting"); setLive("WAITING · Ada approval", "waiting"); showApproval(di); setPlane("sandbox"); setProgress(((di + 1) / events.length) * 100); }
        else { setStatus("Complete", "done"); setLive("Complete · " + events.length + " events", "done"); setPlane(""); setProgress(100); }
        return;
      }
      setStatus("Running", "running");
      setLive("Starting cloud sandbox", "running");
      setProgress(0);
      function step() {
        if (stopped) return;
        if (i >= events.length) { setStatus("Complete", "done"); setLive("Complete · " + events.length + " events", "done"); setPlane(""); setProgress(100); return; }
        var el = traceEl.querySelector('[data-i="' + i + '"]');
        if (el) el.classList.add("in");
        scrollTraceTo(i);
        setPlane(events[i].plane);
        setLive(events[i].plane.toUpperCase() + " · " + events[i].title, "running");
        setProgress(((i + 1) / events.length) * 100);
        if (events[i].type === "decision") {
          pendingIndex = i;
          setStatus("Waiting for approval", "waiting");
          setLive("WAITING · Ada approval", "waiting");
          showApproval(i);
          return;
        }
        i++;
        timers.push(setTimeout(step, STEP_MS));
      }
      step();
    }

    function resolveApproval(ok) {
      if (pendingIndex < 0) return;
      var d = events[pendingIndex];
      d.detail = ok ? "approved by ada · run resumed" : "rejected by ada · no refund";
      d.status = ok ? "ok" : "bad";
      var el = traceEl.querySelector('[data-i="' + pendingIndex + '"]');
      if (el) {
        el.className = "trace-event " + d.status + " in";
        var slot = el.querySelector(".lab-approval-slot");
        if (slot) slot.innerHTML = "";
        var det = el.querySelector(".detail");
        if (det) det.textContent = d.detail;
      }
      var idx = pendingIndex;
      pendingIndex = -1;
      if (stepBtn) stepBtn.disabled = false;
      if (playMode === "step" && ok) {
        setStatus("Step " + stepIndex + "/" + events.length, "idle");
        setLive("Approved · press Next", "idle");
        setPlane("sandbox");
        setProgress((stepIndex / events.length) * 100);
        return;
      }
      if (!ok) {
        events = events.slice(0, idx + 1).concat([
          { plane: "sandbox", type: "tool", title: "tool.call http_fetch", detail: "skipped · supervisor rejected", status: "bad", plugin: "" },
          { plane: "sandbox", type: "model", title: "model.call", detail: "explains that no PR was opened", status: "acc", plugin: "" },
          { plane: "host", type: "cloud", title: "sandbox.destroy", detail: "ephemeral sandbox deleted", status: "ok", plugin: "" },
          { plane: "host", type: "run", title: "run.end", detail: "stop · no PR opened", status: "bad", plugin: "" }
        ]);
        traceEl.innerHTML = traceHTML();
        traceEl.querySelectorAll(".trace-event").forEach(function (e) { e.classList.add("in"); });
        setStatus("Rejected · no PR", "done");
        setLive("Rejected · no PR opened", "done");
        setProgress(100);
        setPlane("");
        return;
      }
      setStatus("Running", "running");
      setLive("Resuming after approval", "running");
      var i = idx + 1;
      if (reduceMotion) {
        traceEl.querySelectorAll(".trace-event").forEach(function (e) { e.classList.add("in"); });
        setStatus("Complete", "done");
        setLive("Complete · " + events.length + " events", "done");
        setProgress(100);
        setPlane("");
        return;
      }
      function step() {
        if (stopped) return;
        if (i >= events.length) { setStatus("Complete", "done"); setLive("Complete · " + events.length + " events", "done"); setPlane(""); setProgress(100); return; }
        var e = traceEl.querySelector('[data-i="' + i + '"]');
        if (e) e.classList.add("in");
        scrollTraceTo(i);
        setPlane(events[i].plane);
        setLive(events[i].plane.toUpperCase() + " · " + events[i].title, "running");
        setProgress(((i + 1) / events.length) * 100);
        i++;
        timers.push(setTimeout(step, STEP_MS));
      }
      step();
    }

    function renderAll(animate) {
      renderRows();
      renderSummary();
      renderConfig();
      renderDiff();
      updatePresets();
      events = labTrace(sel);
      traceEl.innerHTML = traceHTML();
      stepIndex = 0;
      if (pendingIndex >= 0) pendingIndex = -1;
      if (stepBtn) stepBtn.disabled = false;
      if (playMode === "step") {
        traceEl.scrollTop = 0;
        setStatus("Step 0/" + events.length, "idle");
        setLive("Step mode · press Next", "idle");
        setProgress(0);
        setPlane("");
        return;
      }
      if (!animate) {
        traceEl.scrollTop = 0;
        traceEl.querySelectorAll(".trace-event").forEach(function (e) { e.classList.add("in"); });
        setStatus("Ready", "idle");
        setLive("Ready · toggle a plugin", "idle");
        setProgress(0);
        setPlane("");
      } else {
        play(0);
      }
      lastSel = makeSel(selectedIds(sel));
    }

    lab.addEventListener("click", function (e) {
      var t = e.target.closest && e.target.closest("[data-plugin],[data-preset],[data-approve],[data-reject],[data-lab-copy]");
      if (!t) return;
      if (t.hasAttribute("data-lab-copy")) { copyText(labConfig(sel), t, "Copied", "Copy config"); return; }
      if (t.hasAttribute("data-plugin")) {
        var id = t.getAttribute("data-plugin");
        if (sel[id]) delete sel[id]; else sel[id] = true;
        started = true;
        if (window.innerWidth <= 900) setLabView("run");
        renderAll(true);
        return;
      }
      if (t.hasAttribute("data-preset")) {
        var name = t.getAttribute("data-preset");
        sel = makeSel(LAB_PRESETS[name] || []);
        started = true;
        if (window.innerWidth <= 900) setLabView("run");
        renderAll(true);
        return;
      }
      if (t.hasAttribute("data-approve")) resolveApproval(true);
      if (t.hasAttribute("data-reject")) resolveApproval(false);
    });

    if (runBtn) runBtn.addEventListener("click", function () {
      started = true;
      renderAll(playMode === "auto");
    });
    if (stepBtn) stepBtn.addEventListener("click", nextStep);
    if (resetBtn) resetBtn.addEventListener("click", function () { sel = {}; started = true; renderAll(playMode === "auto"); });
    modeBtns.forEach(function (b) {
      b.addEventListener("click", function () {
        playMode = b.getAttribute("data-lab-mode");
        modeBtns.forEach(function (x) { x.setAttribute("aria-pressed", String(x === b)); });
        if (stepBtn) stepBtn.hidden = playMode !== "step";
        if (runBtn) runBtn.textContent = playMode === "step" ? "Restart" : "Run";
        if (playMode === "step") { stopTimers(); renderAll(false); started = true; }
        else { renderAll(true); started = true; }
      });
    });

    tabBtns.forEach(function (b) {
      b.addEventListener("click", function () {
        var name = b.getAttribute("data-lab-tab");
        tabBtns.forEach(function (x) { x.setAttribute("aria-selected", String(x === b)); });
        panes.forEach(function (p) { p.hidden = p.getAttribute("data-lab-pane") !== name; });
      });
    });

    function applyLabView() {
      if (window.innerWidth > 900) {
        viewPanels.forEach(function (p) { p.hidden = false; });
        return;
      }
      viewPanels.forEach(function (p) {
        var v = p.getAttribute("data-lab-view-panel");
        p.hidden = v === "config" ? false : v !== labView;
      });
    }
    function setLabView(name) {
      labView = name;
      viewBtns.forEach(function (b) { b.setAttribute("aria-selected", String(b.getAttribute("data-lab-view") === name)); });
      applyLabView();
    }
    viewBtns.forEach(function (b) {
      b.addEventListener("click", function () { setLabView(b.getAttribute("data-lab-view")); });
    });

    function applyLabMode() {
      if (window.innerWidth > 1240) {
        panes.forEach(function (p) { p.hidden = false; });
      } else {
        var active = lab.querySelector('[data-lab-tab][aria-selected="true"]');
        var name = active ? active.getAttribute("data-lab-tab") : "config";
        panes.forEach(function (p) { p.hidden = p.getAttribute("data-lab-pane") !== name; });
      }
    }
    window.addEventListener("resize", function () { applyLabMode(); applyLabView(); });
    applyLabMode();
    applyLabView();

    labController = {
      stop: function () { stopTimers(); },
      start: function () { if (started && current === "introduction") renderAll(true); }
    };

    renderAll(false);
    if (reduceMotion) {
      started = true;
      renderAll(true);
    } else if ("IntersectionObserver" in window) {
      var labObs = new IntersectionObserver(function (entries) {
        if (entries[0].isIntersecting) {
          labObs.disconnect();
          if (!started) { started = true; renderAll(true); }
        }
      }, { threshold: .18 });
      labObs.observe(lab);
    }
  })();

  // Own the scroll position: the browser's native jump to #page would
  // land under the sticky header.
  try { history.scrollRestoration = "manual"; } catch (e) {}
  current = null;
  // Embed the live demo where it's served next to the docs (Vercel, local).
  (function () {
    var box = document.getElementById("demo-embed");
    if (!box) return;
    var here = /vercel\.app$|^localhost$|^127\.0\.0\.1$/.test(location.hostname);
    var url = here ? "demo/" : "https://eipagent-docs.vercel.app/demo/";
    box.innerHTML = (here ? '<div class="demo-frame"><iframe src="' + url + '" title="Acme Support Desk demo" loading="lazy"></iframe></div>' : "") +
      '<p class="demo-cap">' + (here ? "The live demo, served by the Go backend (on a free instance, so the first visit can take a minute to wake it; until then you can use a replay of recorded runs). Chat as Sam, then switch to Ada to decide. " : "") +
      '<a href="' + url + '">Open the demo in a new tab</a>.</p>';
    // Show the desktop layout scaled to the column, except on phones.
    var frame = box.querySelector(".demo-frame");
    function fit() {
      if (!frame) return;
      var w = frame.clientWidth, scaled = window.innerWidth >= 640;
      frame.classList.toggle("scaled", scaled);
      var ifr = frame.querySelector("iframe");
      ifr.style.transform = scaled ? "scale(" + (w / 1280) + ")" : "";
      frame.style.height = scaled ? Math.round(820 * w / 1280) + "px" : "640px";
    }
    window.addEventListener("resize", fit);
    window.addEventListener("hashchange", function () { setTimeout(fit, 0); });
    document.addEventListener("click", function () { setTimeout(fit, 0); });
    setTimeout(fit, 0);
  })();
  show(location.hash.slice(1));
  var settle = function () { var t = pageFor(location.hash.slice(1)); if (t.anchor) t.anchor.scrollIntoView(); else window.scrollTo(0, 0); };
  var userScrolled = false;
  ["wheel", "touchstart", "keydown"].forEach(function (ev) { window.addEventListener(ev, function () { userScrolled = true; }, { once: true, passive: true }); });
  [0, 120, 400].forEach(function (ms) { setTimeout(function () { if (!userScrolled) settle(); }, ms); });
  window.addEventListener("load", function () { if (!userScrolled) settle(); setTimeout(function () { if (!userScrolled) settle(); }, 60); });
})();
