// 面板前端脚本：分页、参数过滤、搜索框、主题、自动刷新、二次确认、弹窗。
//
// 文案统一走 t() 取译文：译文由服务端按当前语言写进 window.I18N
//（见 internal/web/web.go 的 pageCommon），查不到就原样用中文 —— 不会出现空白或键名。

function t(key) {
  try {
    if (window.I18N && window.I18N[key]) return window.I18N[key];
  } catch (e) { /* 忽略 */ }
  return key;
}
// 设备详情页/概览页上的几处小交互，纯原生 JS，没有依赖。
//
//   1. 表格分页：带 data-pager="20" 的表格默认 20 条/页
//   2. 参数名过滤：过滤之后重新分页（两者是配合关系，不是各干各的）
//   3. 概览页搜索框：防抖自动提交（服务端过滤）
//   4. 破坏性操作（重启设备等）的二次确认
//   5. 终端弹窗：data-modal 打开、点遮罩或 ✕ 或 Esc 关闭
//   6. 首页的 5 秒自动刷新开关（状态记在 localStorage）
//   7. 重启页：面板换了端口，倒计时后自动跳到新地址
document.addEventListener("DOMContentLoaded", function () {
  var pagers = setupPagers(document);
  wireParamFilter(pagers);
  wireSearchBox();
  wireThemeToggle();
  wireAutoRefresh();
  wireConfirms();
  wireModals();
  wireRestartRedirect();
  wireSortable(pagers);
  wireCopyButtons();
  wireSubnav();
  wireHotkeys();
  wireTokenGen();
});

// ---------- 重启页：面板换了端口，倒计时后自己走过去 ----------
//
// 重启响应是**旧进程**发出来的，它马上就会退出，所以不能靠服务端 302
//（连接一断，浏览器可能根本收不到跳转），只能让浏览器自己走。
// 留 3 秒倒计时比瞬间跳走让人安心。
function wireRestartRedirect() {
  var box = document.getElementById("restart-box");
  if (!box) return;
  var url = box.getAttribute("data-restart-url");
  if (!url) return;
  var left = parseInt(box.getAttribute("data-restart-seconds") || "3", 10);
  if (!(left > 0)) left = 3;
  var out = box.querySelector("[data-restart-count]");
  function tick() {
    if (out) out.textContent = String(left);
    if (left <= 0) {
      // replace：别让「后退」又回到那个已经关掉的旧地址
      window.location.replace(url);
      return;
    }
    left--;
    window.setTimeout(tick, 1000);
  }
  tick();
}

// ---------- 5 秒自动刷新（设备列表页 + 设备详情页，开关状态全局共享）----------
//
// 在线/离线、探测中、任务进度这些会自己变，盯着看的时候不用手动刷。
// 开关状态记在 localStorage（**所以两个页面共用一个开关**）：在一个页面打开，
// 切到另一个页面也是开着的，刷新后仍然保持。
// 三种情况会跳过这一轮（下一轮再看）：
//   1. 焦点在输入框里（正在打字）—— 别把没提交的搜索词刷掉；
//   2. 有弹窗开着 —— 别把正在看的终端列表/正在填的无线表单刷没；
//   3. 开着的时候页面不可见？不，后台标签照刷，切回来就是新的。
// ---------- 刷新 / 翻页的骨架反馈 ----------
//
// 自动刷新是整页 reload，页面会先白一下再出内容 —— 在快速轮询下这个空档
// 尤其扎眼（每 5 秒一次）。这里在 reload 之前先铺一层灰条占位：行数与
// 真实表格一致，行高也一致，视觉上是「数据在更新」而不是「页面空了」。
// 同时给顶栏加 .busy，走一条顶部细进度条。
function markBusy(ms) {
  var tables = document.querySelectorAll("table[data-pager]");
  var pending = [];
  tables.forEach(function (table) {
    var tbody = table.tBodies && table.tBodies[0];
    if (!tbody || tbody.children.length === 0) return;
    var cols = table.tHead && table.tHead.rows.length
      ? table.tHead.rows[0].cells.length : 6;
    // 占位行数取 8，不多不少：比真实行多会显得页面在「加载更多」，
    // 比真实行少则刷新瞬间行会消失又出现，反而更跳。
    var frag = document.createDocumentFragment();
    for (var r = 0; r < 8; r++) {
      var tr = document.createElement("tr");
      tr.className = "skel-row";
      for (var c = 0; c < cols; c++) {
        var td = document.createElement("td");
        var bar = document.createElement("span");
        bar.className = "skeleton";
        // 错开每格的起始宽度，灰条长短不一才像真实内容而不是一排等宽砖块
        bar.style.width = (55 + ((r * 7 + c * 13) % 40)) + "%";
        td.appendChild(bar);
        tr.appendChild(td);
      }
      frag.appendChild(tr);
    }
    // 记下真实行，reload 之前不必还原（马上要跳走了）；
    // 万一 reload 被浏览器拦下（如 beforeunload），还能恢复。
    pending.push([tbody, Array.prototype.slice.call(tbody.children)]);
    tbody.replaceChildren(frag);
  });
  document.body.classList.add("busy");
  window.setTimeout(function () {
    document.body.classList.remove("busy");
    pending.forEach(function (p) { p[0].replaceChildren.apply(p[0], p[1]); });
  }, ms || 900);
}

// ---------- 自动刷新 ----------
function wireAutoRefresh() {
  var btn = document.getElementById("autorefresh");
  if (!btn) return;
  var KEY = "acs-autorefresh";
  var SECONDS = 5;
  var on = false;
  var timer = null;
  try { on = localStorage.getItem(KEY) === "1"; } catch (e) { /* 隐私模式下读不到，当关 */ }

  function label() {
    btn.textContent = t(on ? "自动刷新 %ds：开" : "自动刷新 %ds：关").replace("%d", SECONDS);
    btn.classList.toggle("on", on);
    btn.setAttribute("aria-pressed", on ? "true" : "false");
  }

  function schedule() {
    if (timer) { clearTimeout(timer); timer = null; }
    if (!on) return;
    timer = setTimeout(function () {
      var el = document.activeElement;
      if (el && (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.tagName === "SELECT")) {
        schedule();   // 正在输入，跳过这一轮
        return;
      }
      if (document.querySelector(".modal-mask:not([hidden])")) {
        schedule();   // 弹窗开着，跳过这一轮
        return;
      }
      markBusy(700);
      window.location.reload();
    }, SECONDS * 1000);
  }

  label();
  schedule();

  btn.addEventListener("click", function () {
    on = !on;
    try { localStorage.setItem(KEY, on ? "1" : "0"); } catch (e) { /* 忽略 */ }
    label();
    schedule();
  });
}

// ---------- 终端弹窗 ----------
//
// 服务端把弹窗内容一起渲染在页面里（hidden），这里只管开/关 ——
// 不用额外请求，不依赖 JS 也能看到内容（只是要点开而已）。
function wireModals() {
  function close(mask) { mask.hidden = true; }

  document.querySelectorAll("[data-modal]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var mask = document.getElementById(btn.getAttribute("data-modal"));
      if (mask) mask.hidden = false;
    });
  });

  document.querySelectorAll(".modal-mask").forEach(function (mask) {
    // 点遮罩（而不是卡片本身）关闭
    mask.addEventListener("click", function (e) {
      if (e.target === mask) close(mask);
    });
    mask.querySelectorAll("[data-modal-close]").forEach(function (x) {
      x.addEventListener("click", function () { close(mask); });
    });
  });

  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    document.querySelectorAll(".modal-mask").forEach(function (m) { close(m); });
  });
}

// ---------- 破坏性操作的二次确认 ----------
//
// 要确认的文字写在 form 的 data-confirm 属性上，不是拼在 JS 里：
// 属性由模板负责转义（设备名里带引号也不会把脚本搞坏），这里只负责弹框。
function wireConfirms() {
  document.querySelectorAll("form[data-confirm]").forEach(function (form) {
    form.addEventListener("submit", function (e) {
      if (!window.confirm(form.getAttribute("data-confirm"))) {
        e.preventDefault();
      }
    });
  });
}

// ---------- 日间 / 夜间模式 ----------
//
// 主题值在页面 <head> 的肉联脚本里已经写好（避免刷新闪一下），
// 这里只负责按钮的标签与切换。
function wireThemeToggle() {
  var btn = document.getElementById("theme-toggle");
  var root = document.documentElement;
  if (!btn) return;

  function label() {
    btn.textContent = root.getAttribute("data-theme") === "light"
      ? t("🌙 切换到夜间") : t("☀ 切换到日间");
  }
  label();

  btn.addEventListener("click", function () {
    var next = root.getAttribute("data-theme") === "light" ? "dark" : "light";
    root.setAttribute("data-theme", next);
    try {
      localStorage.setItem("theme", next);
    } catch (e) { /* 隐私模式下写不进去，忽略 */ }
    label();
  });
}

// ---------- 表格分页 ----------
//
// 数据本来就全部渲染在 DOM 里了，所以在浏览器端分页就够 ——
// 不用往返服务端，而且能和「参数过滤」天然配合：过滤出子集后重新分页。

function setupPagers(root) {
  var pagers = {};
  Array.prototype.forEach.call(root.querySelectorAll("table[data-pager]"), function (table) {
    var size = parseInt(table.getAttribute("data-pager"), 10);
    if (!size || size < 1) size = 20;
    var pager = makePager(table, size);
    if (table.id) pagers[table.id] = pager;
  });
  return pagers;
}

function makePager(table, defaultSize) {
  var tbody = table.tBodies[0];
  if (!tbody) return null;
  var rows = Array.prototype.slice.call(tbody.rows);

  var size = defaultSize;
  var page = 1;
  var predicate = function () { return true; };

  // 自动刷新是整页重载，所以把"看到第几页 / 每页多少条"记在 sessionStorage 里，
  // 不然每 5 秒就被打回第一页（开着自动刷新看参数表时最难受）。
  var stateKey = table.id ? "acs-pager:" + table.id : "";
  if (stateKey) {
    try {
      var sSize = parseInt(sessionStorage.getItem(stateKey + ":size"), 10);
      var sPage = parseInt(sessionStorage.getItem(stateKey + ":page"), 10);
      if (!isNaN(sSize) && sSize >= 0) size = sSize;
      if (!isNaN(sPage) && sPage > 0) page = sPage;
    } catch (e) { /* 隐私模式读不到，用默认值 */ }
  }

  // 行数本来就不到一页时，分页条纯属噪音，直接不显示。
  if (rows.length <= defaultSize) {
    return {
      setFilter: function () {},
      render: function () {},
      rescan: function () {},
    };
  }

  var bar = document.createElement("div");
  bar.className = "pager";

  var prev = mkBtn(t("‹ 上一页"));
  var next = mkBtn(t("下一页 ›"));
  var info = document.createElement("span");
  info.className = "pager-info";

  var sizeSel = document.createElement("select");
  sizeSel.className = "pager-size";
  [20, 50, 100, 0].forEach(function (n) {
    var o = document.createElement("option");
    o.value = String(n);
    o.textContent = n === 0 ? t("全部") : t("每页 %d 条").replace("%d", n);
    sizeSel.appendChild(o);
  });
  sizeSel.value = String(size);

  bar.appendChild(prev);
  bar.appendChild(info);
  bar.appendChild(next);
  bar.appendChild(sizeSel);
  table.parentNode.insertBefore(bar, table.nextSibling);

  function mkBtn(text) {
    var b = document.createElement("button");
    b.type = "button";
    b.className = "ghost";
    b.textContent = text;
    return b;
  }

  function render() {
    var list = rows.filter(predicate);
    var pages = size > 0 ? Math.max(1, Math.ceil(list.length / size)) : 1;
    if (page > pages) page = pages;
    if (page < 1) page = 1;

    var startIdx = size > 0 ? (page - 1) * size : 0;
    var endIdx = size > 0 ? Math.min(startIdx + size, list.length) : list.length;

    // 先全部藏起来，再只显示本页的 —— 比逐行切换简单且不会漏
    for (var i = 0; i < rows.length; i++) rows[i].style.display = "none";
    for (var j = startIdx; j < endIdx; j++) list[j].style.display = "";

    info.textContent = size > 0
      ? t("共 %d 条 · 第 %d / %d 页").replace("%d", list.length).replace("%d", page).replace("%d", pages)
      : t("共 %d 条").replace("%d", list.length);
    prev.disabled = page <= 1;
    next.disabled = page >= pages;

    if (stateKey) {
      try {
        sessionStorage.setItem(stateKey + ":page", String(page));
        sessionStorage.setItem(stateKey + ":size", String(size));
      } catch (e) { /* 忽略 */ }
    }
  }

  prev.addEventListener("click", function () { page--; render(); });
  next.addEventListener("click", function () { page++; render(); });
  sizeSel.addEventListener("change", function () {
    size = parseInt(sizeSel.value, 10);
    page = 1;
    render();
  });

  render();

  return {
    // setFilter 会重置到第 1 页 —— 过滤后还停在第 47 页会很困惑
    setFilter: function (fn) { predicate = fn; page = 1; render(); },
    render: render,
    // rescan 给「表头排序」用：行在 DOM 里被重新排过之后，重新按当前
    // DOM 顺序拿一遍行列表，再从第 1 页开始渲染 —— 不重扫的话分页还
    // 按初始化时的旧顺序切页，排序就像没生效一样。
    rescan: function () {
      rows = Array.prototype.slice.call(tbody.rows);
      page = 1;
      render();
    },
  };
}

// ---------- 参数名过滤 ----------

function wireParamFilter(pagers) {
  var box = document.getElementById("param-filter");
  if (!box) return;
  var pager = pagers["param-table"];

  function apply() {
    var q = box.value.trim().toLowerCase();
    var fn = function (tr) {
      var name = tr.getAttribute("data-name") || "";
      return !q || name.toLowerCase().indexOf(q) >= 0;
    };
    if (pager) {
      pager.setFilter(fn);
    } else {
      // 兜底：没有分页条时就直接显隐
      Array.prototype.forEach.call(
        document.querySelectorAll("#param-table tbody tr"),
        function (tr) { tr.style.display = fn(tr) ? "" : "none"; }
      );
    }
  }

  // 过滤词同样记在 sessionStorage：自动刷新重载后还在，不用每 5 秒重打一遍
  var filterKey = "acs-param-filter";
  try {
    var savedFilter = sessionStorage.getItem(filterKey);
    if (savedFilter) box.value = savedFilter;
  } catch (e) { /* 忽略 */ }

  box.addEventListener("input", function () {
    try { sessionStorage.setItem(filterKey, box.value); } catch (e) { /* 忽略 */ }
    apply();
  });
  apply();
}

// ---------- 概览页搜索框 ----------
// 输入后停顿一下自动提交（服务端过滤，结果 URL 可分享）。
// 刷新后把光标放回输入框，让连续输入不被打断。
function wireSearchBox() {
  var box = document.getElementById("dev-search");
  if (!box || !box.form) return;

  if (box.value) {
    box.focus();
    try {
      box.setSelectionRange(box.value.length, box.value.length);
    } catch (e) { /* 某些浏览器对非 text 类型不支持，忽略 */ }
  }

  var timer = null;
  box.addEventListener("input", function () {
    clearTimeout(timer);
    timer = setTimeout(function () { box.form.submit(); }, 350);
  });
}

// ---------- 表头排序（概览页设备表） ----------
//
// 点表头按这一列排序：数字列（参数数 / 终端数 / 收发光读数）按数值比，
// 时间列（2026-10-11 17:01:18 这种格式）和文本列按字符串比，「-」和空格
// 永远排在最后（没有数据的设备不该冒充最大或最小）。再点一次反向。
//
// 排序就是老老实实重排 DOM 行，然后让分页器 rescan —— 分页、过滤、排序
// 三个交互各管各的，靠 rescan 对齐，谁也不用知道谁的实现。
function wireSortable(pagers) {
  document.querySelectorAll("table.sortable").forEach(function (table) {
    var tbody = table.tBodies[0];
    var head = table.tHead;
    if (!tbody || !head || !head.rows.length) return;
    Array.prototype.forEach.call(head.rows[0].cells, function (th, idx) {
      // 没有列名的表头（操作列）不参与排序
      if (!th.textContent.trim()) return;
      th.classList.add("sortable-th");
      th.addEventListener("click", function () {
        sortTableByColumn(table, tbody, idx, th, pagers);
      });
    });
  });
}

function sortTableByColumn(table, tbody, idx, th, pagers) {
  var dir = th.getAttribute("data-dir") === "asc" ? "desc" : "asc";
  // 清掉其他列的排序标记 —— 一次只有一列在排序
  Array.prototype.forEach.call(table.tHead.rows[0].cells, function (c) {
    if (c !== th) c.removeAttribute("data-dir");
  });
  th.setAttribute("data-dir", dir);

  var texts = Array.prototype.map.call(tbody.rows, function (tr) {
    var td = tr.cells[idx];
    return td ? td.textContent.trim() : "";
  });
  var kind = classifyColumn(texts);
  var rows = Array.prototype.slice.call(tbody.rows);
  rows.sort(function (a, b) {
    var ka = sortKey(a.cells[idx], kind);
    var kb = sortKey(b.cells[idx], kind);
    // 空值（"-" / 空列）永远排最后，跟方向无关
    if (ka === null && kb === null) return 0;
    if (ka === null) return 1;
    if (kb === null) return -1;
    var cmp = kind === "num" ? ka - kb : (ka < kb ? -1 : ka > kb ? 1 : 0);
    return dir === "asc" ? cmp : -cmp;
  });
  rows.forEach(function (tr) { tbody.appendChild(tr); });

  var pager = pagers[table.id];
  if (pager && pager.rescan) pager.rescan();
}

// 这一列按什么口径比：所有非空格都是「数字 + 可选单位」就按数值，
// 否则按文本（时间戳 2026-10-11 17:01:18 的字符串序恰好就是时间序）。
function classifyColumn(texts) {
  var sawValue = false;
  for (var i = 0; i < texts.length; i++) {
    var s = texts[i];
    if (!s || s === "-") continue;
    sawValue = true;
    if (!/^-?\d+(\.\d+)?(\s*(dBm|℃|mA|V|%))?$/.test(s)) return "text";
  }
  return sawValue ? "num" : "text";
}

function sortKey(td, kind) {
  if (!td) return null;
  var s = td.textContent.trim();
  if (!s || s === "-") return null;
  if (kind === "num") {
    var n = parseFloat(s);
    return isNaN(n) ? null : n;
  }
  return s.toLowerCase();
}

// ---------- 一键复制（序列号这类要拿去别处粘贴的值） ----------
//
// data-copy 属性里放要复制的文本（模板转义，带引号也不会坏）。
// clipboard API 不可用（非安全上下文/旧浏览器）时退回 execCommand。
function wireCopyButtons() {
  document.querySelectorAll("[data-copy]").forEach(function (btn) {
    btn.addEventListener("click", function (e) {
      e.preventDefault();
      var text = btn.getAttribute("data-copy") || "";
      if (!text) return;
      var done = function () {
        btn.classList.add("copied");
        btn.textContent = "✓";
        setTimeout(function () {
          btn.classList.remove("copied");
          btn.textContent = "⧉";
        }, 1200);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function () {
          fallbackCopy(text);
          done();
        });
      } else {
        fallbackCopy(text);
        done();
      }
    });
  });
}

function fallbackCopy(text) {
  var ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand("copy"); } catch (e) { /* 尽力而为 */ }
  document.body.removeChild(ta);
}

// ---------- 详情页区块子导航 ----------
//
// 设备详情页区块多（WAN/基本信息/光功率/FTTR/诊断/无线/参数），铺一排锚点
// 链接省得一路滚。锚点 id 在这里**运行时生成**，模板里的 h2 保持原样 ——
// e2e 有多处按「<h2>」字面量切片截区块的断言，模板里加属性会把它们全弄坏。
function wireSubnav() {
  var main = document.querySelector("main[data-subnav]");
  if (!main) return;
  var sections = main.querySelectorAll("h2");
  if (sections.length < 2) return;
  var bar = document.createElement("nav");
  bar.className = "subnav";
  Array.prototype.forEach.call(sections, function (h, i) {
    if (!h.id) h.id = "sec-" + (i + 1);
    var a = document.createElement("a");
    a.href = "#" + h.id;
    a.textContent = h.textContent;
    bar.appendChild(a);
  });
  main.insertBefore(bar, main.firstChild);
}

// ---------- 快捷键：/ 聚焦概览页搜索框 ----------
function wireHotkeys() {
  document.addEventListener("keydown", function (e) {
    if (e.key !== "/" || e.ctrlKey || e.metaKey || e.altKey) return;
    var el = document.activeElement;
    if (el && (el.tagName === "INPUT" || el.tagName === "TEXTAREA" ||
               el.tagName === "SELECT" || el.isContentEditable)) return;
    if (document.querySelector(".modal-mask:not([hidden])")) return;
    var box = document.getElementById("dev-search");
    if (!box) return;
    e.preventDefault();
    box.focus();
    box.select();
  });
}

// ---------- API Token 现场生成（设置页） ----------
//
// 点「生成」往输入框里填一个 36 位随机十六进制串（18 字节，crypto API）。
// 只是替管理员省一次 openssl rand -hex 的搬运，值仍由表单正常提交、服务端校验。
function wireTokenGen() {
  var btn = document.getElementById("gen-token");
  if (!btn) return;
  btn.addEventListener("click", function () {
    var input = document.querySelector('input[name="api_token"]');
    if (!input) return;
    var bytes = new Uint8Array(18);
    if (window.crypto && window.crypto.getRandomValues) {
      window.crypto.getRandomValues(bytes);
    } else {
      for (var i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256);
    }
    var hex = "";
    for (var j = 0; j < bytes.length; j++) hex += ("0" + bytes[j].toString(16)).slice(-2);
    input.value = hex;
  });
}
