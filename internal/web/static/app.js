"use strict";
// Small helpers for the laptop-backup UI. No framework, no build step.

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

function toast(text, bad) {
  const t = $("#toast");
  t.textContent = text;
  t.className = "flash " + (bad ? "bad" : "ok");
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => (t.hidden = true), 6000);
}

// api posts JSON with the header the server requires for state changes.
async function api(path, body) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Backup-UI": "1" },
    body: JSON.stringify(body || {}),
  });
  const data = await res.json().catch(() => ({ ok: false, error: res.statusText }));
  if (!data.ok) throw new Error(data.error || res.statusText);
  return data;
}

function on(sel, event, fn) {
  $$(sel).forEach((el) =>
    el.addEventListener(event, async (e) => {
      e.preventDefault();
      el.disabled = true;
      try {
        await fn(el, e);
      } catch (err) {
        toast(err.message, true);
      } finally {
        el.disabled = false;
      }
    })
  );
}

function fmtBytes(n) {
  const u = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + " " + u[i];
}

// --- dashboard ---------------------------------------------------------
on("[data-action=run]", "click", async () => { await api("/api/run"); location.reload(); });
on("[data-action=full]", "click", async () => {
  if (!confirm("สร้างดัชนีใหม่จาก Google Drive แล้วซิงก์ทั้งหมด?\nไฟล์บน Drive ที่ไม่มีในเครื่องจะถูกย้ายไปถังขยะ")) return;
  await api("/api/run", { full: true });
  location.reload();
});
on("[data-action=cancel]", "click", async () => { await api("/api/cancel"); toast("กำลังยกเลิก…"); });

const phases = { rescan: "(อ่านรายการจาก Drive)", sync: "", trash: "(ย้ายไฟล์ที่ลบไปถังขยะ)" };
let wasRunning = document.body.dataset.running === "true";

async function poll() {
  let delay = 20000;
  try {
    const res = await fetch("/api/status", { cache: "no-store" });
    const s = await res.json();
    const run = s.run;
    $$("[data-next]").forEach((el) => (el.textContent = s.next));
    // A run started by the schedule (or finished) while the page is open.
    $$("[data-action=run], [data-action=full]").forEach((el) => (el.hidden = run.running));
    $$("[data-action=cancel]").forEach((el) => (el.hidden = !run.running));
    const prog = $("#progress");
    if (prog) {
      prog.hidden = !run.running;
      if (run.running) {
        for (const k of ["scanned", "uploaded", "updated", "trashed", "errors"]) $(`[data-p=${k}]`).textContent = run.stats[k];
        $("[data-p=bytes]").textContent = fmtBytes(run.stats.bytes);
        $("[data-p=current]").textContent = run.stats.current;
        $("[data-p=phase]").textContent = phases[run.stats.phase] || "";
        $("[data-p=paused]").hidden = !run.stats.paused;
      }
    }
    if (run.running !== wasRunning && document.body.dataset.page === "dashboard") {
      // A run that ends can also revoke the Google connection; the auth
      // card is only rendered server-side.
      if (!run.running && String(s.auth.connected) !== document.body.dataset.connected) location.reload();
      else refreshHistory().catch(() => {});
    }
    wasRunning = run.running;
    if (run.running) delay = 2000;
  } catch (e) {
    delay = 30000;
  }
  setTimeout(poll, delay);
}
if (!document.hidden) poll();
else document.addEventListener("visibilitychange", poll, { once: true });

// --- dashboard: run history ---------------------------------------------
// The server renders the first page; older pages come from /api/runs when
// the sentinel row at the bottom of the scroll box comes into view.
const histBox = $("#history");
const histRows = $("#history-rows");
const histMore = $("#history-more");
let histNext = histRows ? histRows.dataset.cursor : "";
let histState = histNext ? "idle" : "done"; // idle | loading | error | done

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

const rowFor = (id) => histRows.querySelector(`tr[data-run="${Number(id)}"]`);

// runRow builds the same row as {{range .Runs}} in dashboard.html, with
// textContent only: run messages may contain anything.
function runRow(r) {
  const tr = el("tr");
  tr.dataset.run = r.id;
  const started = el("td", "", r.started);
  started.append(el("br"), el("small", "muted", r.trigger_label));
  const status = el("td");
  const badge = el("span", "badge", r.status_label);
  if (/^[a-z]+$/.test(r.status)) badge.classList.add(r.status);
  status.append(badge);
  if (r.message) status.append(el("br"), el("small", "muted", r.message));
  const errors = el("td");
  if (r.errors) {
    const a = el("a", "", String(r.errors));
    a.href = "/logs?run=" + encodeURIComponent(r.id);
    errors.append(a);
  } else {
    errors.textContent = "0";
  }
  tr.append(started, status, el("td", "", String(r.uploaded)), el("td", "", String(r.updated)),
    el("td", "", String(r.trashed)), errors, el("td", "", r.size), el("td", "", r.duration));
  return tr;
}

function setHistState(state) {
  histState = state;
  if (state === "error") {
    const retry = el("button", "linkish", "โหลดไม่สำเร็จ — ลองใหม่");
    retry.type = "button";
    retry.onclick = loadOlder;
    histMore.replaceChildren(retry);
  } else {
    histMore.textContent = { loading: "กำลังโหลด…", done: "แสดงครบทุกรายการแล้ว" }[state] || "";
  }
}

async function getRuns(query) {
  const res = await fetch("/api/runs" + query, { cache: "no-store" });
  const d = await res.json();
  if (!res.ok || !d.ok) throw new Error(d.error || res.statusText);
  return d;
}

async function loadOlder() {
  // One request at a time; after an error only the retry button loads.
  if (histState === "loading" || histState === "done") return;
  setHistState("loading");
  try {
    const d = await getRuns("?before=" + encodeURIComponent(histNext));
    histRows.append(...d.runs.filter((r) => !rowFor(r.id)).map(runRow));
    histNext = d.next;
    setHistState(histNext ? "idle" : "done");
  } catch (e) {
    setHistState("error");
    return;
  }
  // Observing again reports whether the sentinel is still in view (a page
  // that did not fill the box), which then loads the next page.
  if (histObserver) {
    histObserver.unobserve(histMore);
    histObserver.observe(histMore);
  }
}

const histObserver = histBox && "IntersectionObserver" in window
  ? new IntersectionObserver((entries) => {
    if (histState === "idle" && entries.some((e) => e.isIntersecting)) loadOlder();
  }, { root: histBox, rootMargin: "0px 0px 120px 0px" })
  : null;
if (histObserver) histObserver.observe(histMore);

// refreshHistory adds runs newer than the top row and updates rows that
// changed (a run that was still running), without moving what the user
// is looking at.
async function refreshHistory() {
  if (!histRows) { location.reload(); return; } // first run: no table yet
  const d = await getRuns("");
  const top = histRows.firstElementChild ? Number(histRows.firstElementChild.dataset.run) : 0;
  const fresh = [];
  for (const r of d.runs) {
    const old = rowFor(r.id);
    if (old) old.replaceWith(runRow(r));
    else if (r.id > top) fresh.push(runRow(r));
  }
  if (!fresh.length) return;
  const height = histBox.scrollHeight;
  histRows.prepend(...fresh);
  if (histBox.scrollTop > 0) histBox.scrollTop += histBox.scrollHeight - height;
}

// --- folders -----------------------------------------------------------
on("[data-remove]", "click", async (el) => {
  if (!confirm("นำโฟลเดอร์นี้ออกจากรายการ?\nสำเนาบน Google Drive จะยังอยู่ ไม่ถูกลบ")) return;
  await api("/api/folders/remove", { path: el.dataset.remove });
  location.reload();
});

on("[data-preview]", "click", async (el) => {
  const out = $$("[data-preview-out]").find((o) => o.dataset.previewOut === el.dataset.preview);
  out.textContent = "กำลังนับ…";
  const res = await fetch("/api/preview?path=" + encodeURIComponent(el.dataset.preview));
  const d = await res.json();
  if (!d.ok) { out.textContent = d.error; return; }
  out.textContent = `${d.files.toLocaleString()} ไฟล์ · ${d.human}` + (d.partial ? " (นับไม่ครบ ใช้เวลานานเกิน)" : "") +
    (d.unreadable ? ` · อ่านไม่ได้ ${d.unreadable}` : "");
});

const browser = $("#browser");
let browsePath = browser ? browser.dataset.start : "";
let browseParent = "";

async function browse(path) {
  const res = await fetch("/api/browse?path=" + encodeURIComponent(path));
  const d = await res.json();
  if (!d.ok) { toast(d.error, true); return; }
  browsePath = d.path;
  browseParent = d.parent;
  $("#browse-path").textContent = d.display;
  const list = $("#browse-list");
  list.replaceChildren(...d.dirs.map((name) => {
    const li = document.createElement("li");
    li.textContent = "📁 " + name;
    li.onclick = () => browse(d.path.replace(/\/$/, "") + "/" + name);
    return li;
  }));
  if (!d.dirs.length) list.innerHTML = '<li class="muted">ไม่มีโฟลเดอร์ย่อย</li>';
}
if (browser) browse(browsePath);
on("[data-browse-up]", "click", () => browse(browseParent));
on("[data-browse-pick]", "click", async () => {
  await api("/api/folders/add", { path: browsePath });
  location.reload();
});

on("[data-save-excludes]", "click", async () => {
  await api("/api/excludes", { patterns: $("#excludes").value });
  toast("บันทึกแล้ว");
});

// --- settings ----------------------------------------------------------
on("#schedule", "submit", async (form) => {
  const f = new FormData(form);
  await api("/api/schedule", {
    mode: f.get("mode"),
    every_hours: parseInt(f.get("every_hours"), 10) || 0,
    daily_at: f.get("daily_at"),
  });
  toast("บันทึกตารางเวลาแล้ว");
  setTimeout(() => location.reload(), 600);
});

on("#upload", "submit", async (form) => {
  const f = new FormData(form);
  await api("/api/upload", {
    upload_workers: parseInt(f.get("upload_workers"), 10) || 0,
    upload_limit_kbps: parseInt(f.get("upload_limit_kbps"), 10) || 0,
  });
  toast("บันทึกการตั้งค่าการอัปโหลดแล้ว");
});

on("#drive", "submit", async (form) => {
  await api("/api/drive-folder", { folder_id: new FormData(form).get("folder_id") });
  toast("บันทึกแล้ว");
  // Drop the "not set yet" notice.
  if ($("#no-drive-folder")) setTimeout(() => location.reload(), 600);
});

on("[data-test-drive]", "click", async () => {
  const out = $("#drive-test");
  out.textContent = "กำลังทดสอบ…";
  try {
    const d = await api("/api/drive-folder/test", { folder_id: $("#drive [name=folder_id]").value });
    out.textContent = "✔ เข้าถึงได้: " + d.name;
  } catch (err) {
    out.textContent = "✘ " + err.message;
  }
});

on("[data-disconnect]", "click", async () => {
  if (!confirm("ยกเลิกการเชื่อมต่อบัญชี Google? (ลบ token ในเครื่องนี้)")) return;
  await api("/api/disconnect");
  location.href = "/settings";
});

// --- logs --------------------------------------------------------------
async function loadLog() {
  const pre = $("#log");
  if (!pre) return;
  const res = await fetch("/api/log", { cache: "no-store" });
  pre.textContent = (await res.text()) || "(ว่าง)";
  pre.scrollTop = pre.scrollHeight;
}
loadLog();
on("[data-reload-log]", "click", loadLog);
