"use strict";
// Dependency-free SVG charts for the dashboard "การใช้งาน" section. No
// library, no CDN, no build step. Every chart is an inline <svg> with a
// viewBox that matches its container's width (redrawn when it changes), so
// text stays readable at phone width. Colours come from CSS custom
// properties (app.css) through classes, so light/dark just works.
// All text is set with textContent / SVG text nodes; data is never parsed as markup.

const Charts = (() => {
  const NS = "http://www.w3.org/2000/svg";
  const MONTHS = ["ม.ค.", "ก.พ.", "มี.ค.", "เม.ย.", "พ.ค.", "มิ.ย.", "ก.ค.", "ส.ค.", "ก.ย.", "ต.ค.", "พ.ย.", "ธ.ค."];
  const STATUSES = ["success", "partial", "failed", "cancelled", "interrupted"];

  function svgEl(tag, attrs, text) {
    const e = document.createElementNS(NS, tag);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    if (text !== undefined) e.textContent = text;
    return e;
  }

  function html(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  // Same units as the Go side (fmtBytes): "512 B", "1.5 KiB", "3.2 GiB".
  function bytes(n, trim) {
    const u = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    const s = i ? n.toFixed(1) : String(Math.round(n));
    return (trim ? s.replace(/\.0$/, "") : s) + " " + u[i];
  }

  // Like Go's time.Duration.String for whole seconds: 45s, 1m30s, 1h5m0s.
  function duration(sec) {
    sec = Math.round(sec);
    const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
    return (h ? h + "h" : "") + (h || m ? m + "m" : "") + s + "s";
  }

  const count = (n) => Number(n).toLocaleString("th-TH");

  function shortDate(iso) { // 2026-10-28 -> 28 ต.ค.
    const [, m, d] = iso.split("-").map(Number);
    return d + " " + MONTHS[m - 1];
  }

  function longDate(iso) {
    const [y, m, d] = iso.split("-").map(Number);
    return d + " " + MONTHS[m - 1] + " " + y;
  }

  // ticks returns 0, step, 2*step ... up to the first value >= max. The step
  // is 1, 2 or 5 times a power of ten, and at least one unit (so byte axes
  // step in whole KiB/MiB and counts in whole files).
  function ticks(max, unit, n = 4) {
    const raw = max > 0 ? max / unit / n : 1;
    const pow = Math.pow(10, Math.floor(Math.log10(raw)));
    const f = raw / pow;
    const step = Math.max(1, (f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10) * pow) * unit;
    const out = [0];
    while (out[out.length - 1] < max || out.length < 2) out.push(out[out.length - 1] + step);
    return out;
  }

  function byteUnit(max) {
    let unit = 1;
    while (max >= unit * 1024 * 1.5 && unit < 1024 ** 5) unit *= 1024;
    return unit;
  }

  // --- tooltips ----------------------------------------------------------
  // Hit areas carry a ._tip (array of lines). Hovering shows the tooltip;
  // tapping (click) pins it until the next tap elsewhere, so touch works.

  function wire(box, svg) {
    const tip = html("div", "chart-tip");
    tip.setAttribute("role", "tooltip");
    tip.hidden = true;
    let sel = null;
    const hide = () => {
      tip.hidden = true;
      if (sel) sel.classList.remove("sel");
      sel = null;
    };
    const show = (t) => {
      if (sel) sel.classList.remove("sel");
      sel = t;
      t.classList.add("sel");
      tip.replaceChildren(...t._tip.map((line, i) => html("div", i ? "" : "tip-h", line)));
      tip.hidden = false;
      const b = box.getBoundingClientRect(), r = t.getBoundingClientRect();
      const x = r.left + r.width / 2 - b.left - tip.offsetWidth / 2;
      tip.style.left = Math.max(0, Math.min(x, b.width - tip.offsetWidth)) + "px";
      const above = r.top - b.top - tip.offsetHeight - 6;
      tip.style.top = (above >= 0 ? above : r.bottom - b.top + 6) + "px";
    };
    let pinned = false;
    svg.addEventListener("pointerover", (e) => {
      if (e.pointerType === "mouse" && e.target._tip && !pinned) show(e.target);
    });
    svg.addEventListener("pointerout", (e) => {
      if (e.pointerType === "mouse" && !pinned) hide();
    });
    svg.addEventListener("click", (e) => {
      e.stopPropagation();
      closeOthers(tip);
      if (e.target._tip && !(pinned && sel === e.target)) { pinned = true; show(e.target); } else { pinned = false; hide(); }
    });
    tip._reset = () => { pinned = false; hide(); };
    box.append(svg, tip);
  }

  function closeOthers(keep) {
    document.querySelectorAll(".chart-tip").forEach((t) => { if (t !== keep && t._reset) t._reset(); });
  }
  document.addEventListener("click", () => closeOthers(null));

  // --- mounting and resizing ---------------------------------------------

  const ro = "ResizeObserver" in window
    ? new ResizeObserver((entries) => {
      for (const e of entries) {
        const box = e.target;
        const w = Math.floor(box.clientWidth);
        if (box._build && w > 0 && w !== box._w) paint(box);
      }
    })
    : null;

  // mount draws build(width) into box and redraws when the width changes.
  // build returns an <svg>, or null for "no data".
  function mount(box, build) {
    box._build = build;
    paint(box);
    if (ro && !box._observed) { box._observed = true; ro.observe(box); }
  }

  function paint(box) {
    const w = Math.max(220, Math.floor(box.clientWidth) || 600);
    box._w = Math.floor(box.clientWidth);
    box.replaceChildren();
    const svg = box._build(w);
    if (!svg) {
      box.append(html("p", "muted empty", "ยังไม่มีข้อมูล"));
      return;
    }
    wire(box, svg);
  }

  function note(box, text) {
    box._build = null;
    box.replaceChildren(html("p", "muted empty", text));
  }

  // --- vertical (stacked) bars ---------------------------------------------
  // o: {items:[{label, segs:[{v, cls}], tip:[...], mark}], unit, fmt, aria}

  function vbars(w, o) {
    const h = 210, m = { l: 56, r: 8, t: 16, b: 24 };
    const iw = w - m.l - m.r, ih = h - m.t - m.b;
    const sum = (it) => it.segs.reduce((a, s) => a + s.v, 0);
    const max = Math.max(0, ...o.items.map(sum));
    const t = ticks(max, o.unit);
    const top = t[t.length - 1];
    const y = (v) => m.t + ih - (v / top) * ih;
    const svg = svgEl("svg", { class: "chart-svg", viewBox: `0 0 ${w} ${h}`, role: "img", "aria-label": o.aria });

    for (const v of t) {
      svg.append(svgEl("line", { class: v ? "grid" : "axis", x1: m.l, x2: w - m.r, y1: y(v), y2: y(v) }));
      svg.append(svgEl("text", { class: "tick", x: m.l - 6, y: y(v) + 3.5, "text-anchor": "end" }, o.fmt(v)));
    }

    const n = o.items.length, slot = iw / n, bw = Math.max(2, Math.min(slot * 0.72, 40));
    const every = Math.ceil(46 / slot); // thin the date labels out
    o.items.forEach((it, i) => {
      const cx = m.l + (i + 0.5) * slot;
      let base = m.t + ih;
      for (const s of it.segs) {
        if (s.v <= 0) continue;
        const sh = Math.max((s.v / top) * ih, 1);
        base -= sh;
        svg.append(svgEl("rect", { class: "seg " + s.cls, x: cx - bw / 2, y: base, width: bw, height: sh }));
      }
      if (it.mark) svg.append(svgEl("circle", { class: "mark-err", cx, cy: base - 6, r: 3.3 }));
      if ((n - 1 - i) % every === 0) {
        // The last label would be cut off by the right edge: right-align it.
        const atEdge = cx + 20 > w;
        svg.append(svgEl("text", { class: "tick", x: atEdge ? w - 1 : cx, y: h - 7, "text-anchor": atEdge ? "end" : "middle" }, it.label));
      }
      const hit = svgEl("rect", { class: "hit", x: m.l + i * slot, y: m.t - 12, width: slot, height: ih + 12 });
      hit._tip = it.tip;
      svg.append(hit);
    });
    return svg;
  }

  function daily(box, days) {
    const total = days.reduce((a, d) => a + d.bytes, 0);
    if (!total) return note(box, "ยังไม่มีข้อมูล");
    const peak = days.reduce((a, d) => (d.bytes > a.bytes ? d : a));
    const aria = `แผนภูมิแท่ง: ปริมาณที่อัปโหลดต่อวัน ${days.length} วันล่าสุด รวม ${bytes(total)} ` +
      `มากที่สุด ${bytes(peak.bytes)} ในวันที่ ${longDate(peak.day)}`;
    mount(box, (w) => {
      const unit = byteUnit(peak.bytes);
      return vbars(w, {
        unit, aria, fmt: (v) => bytes(v, true),
        items: days.map((d) => ({
          label: shortDate(d.day), segs: [{ v: d.bytes, cls: "s-acc" }],
          tip: [longDate(d.day), "อัปโหลด " + bytes(d.bytes)],
        })),
      });
    });
  }

  function runLabel(r) { return shortDate(r.started.slice(0, 10)); }

  function changes(box, runs) {
    if (!runs.length) return note(box, "ยังไม่มีข้อมูล");
    const sum = (k) => runs.reduce((a, r) => a + r[k], 0);
    const aria = `แผนภูมิแท่งซ้อน: ไฟล์ต่อรอบ ${runs.length} รอบล่าสุด อัปโหลดใหม่ ${sum("uploaded")} ` +
      `อัปเดต ${sum("updated")} ย้ายไปถังขยะ ${sum("trashed")} รอบที่มีข้อผิดพลาด ${runs.filter((r) => r.errors).length}`;
    mount(box, (w) => vbars(w, {
      unit: 1, aria, fmt: count,
      items: runs.map((r) => ({
        label: runLabel(r),
        segs: [{ v: r.uploaded, cls: "s-new" }, { v: r.updated, cls: "s-upd" }, { v: r.trashed, cls: "s-del" }],
        mark: r.errors > 0,
        tip: [`รอบ #${r.id} · ${r.status_label}`, r.started,
          `ใหม่ ${count(r.uploaded)} · อัปเดต ${count(r.updated)} · ถังขยะ ${count(r.trashed)}`,
          r.errors ? `ผิดพลาด ${count(r.errors)}` : "ไม่มีข้อผิดพลาด"],
      })),
    }));
  }

  function durations(box, runs) {
    if (!runs.length) return note(box, "ยังไม่มีข้อมูล");
    const longest = runs.reduce((a, r) => (r.duration_sec > a.duration_sec ? r : a));
    const aria = `แผนภูมิแท่ง: ระยะเวลาต่อรอบ ${runs.length} รอบล่าสุด นานที่สุด ${duration(longest.duration_sec)} ` +
      `(รอบ #${longest.id}) สีแสดงสถานะ`;
    mount(box, (w) => {
      const max = longest.duration_sec;
      const unit = max < 120 ? 1 : max < 7200 ? 60 : 3600;
      const suffix = { 1: "s", 60: "m", 3600: "h" }[unit];
      return vbars(w, {
        unit, aria, fmt: (v) => v / unit + suffix,
        items: runs.map((r) => ({
          label: runLabel(r),
          segs: [{ v: r.duration_sec, cls: "st-" + (STATUSES.includes(r.status) ? r.status : "cancelled") }],
          tip: [`รอบ #${r.id} · ${r.status_label}`, r.started, "ใช้เวลา " + duration(r.duration_sec)],
        })),
      });
    });
  }

  // --- horizontal bars: backed-up data per source folder ---------------------

  function sources(box, list) {
    if (!list.length) return note(box, "ยังไม่มีข้อมูล");
    const rows = [...list].sort((a, b) => b.bytes - a.bytes);
    const aria = "แผนภูมิแท่งแนวนอน: ข้อมูลที่สำรองไว้ตามโฟลเดอร์ " +
      rows.map((s) => `${s.display} ${bytes(s.bytes)} ${count(s.files)} ไฟล์`).join(", ");
    mount(box, (w) => {
      const rh = 42, max = rows[0].bytes;
      const svg = svgEl("svg", { class: "chart-svg", viewBox: `0 0 ${w} ${rows.length * rh}`, role: "img", "aria-label": aria });
      rows.forEach((s, i) => {
        const y0 = i * rh;
        const value = `${bytes(s.bytes)} · ${count(s.files)} ไฟล์`;
        const room = Math.max(8, Math.floor((w - value.length * 6.6 - 12) / 6.4));
        const label = s.display.length > room ? "…" + s.display.slice(s.display.length - room + 1) : s.display;
        svg.append(svgEl("text", { class: "lbl", x: 0, y: y0 + 14 }, label));
        svg.append(svgEl("text", { class: "val", x: w, y: y0 + 14, "text-anchor": "end" }, value));
        svg.append(svgEl("rect", { class: "track", x: 0, y: y0 + 22, width: w, height: 10, rx: 3 }));
        const bw = max > 0 ? Math.max((s.bytes / max) * w, s.bytes > 0 ? 2 : 0) : 0;
        if (bw) svg.append(svgEl("rect", { class: "seg s-acc", x: 0, y: y0 + 22, width: bw, height: 10, rx: 3 }));
        const hit = svgEl("rect", { class: "hit", x: 0, y: y0, width: w, height: rh });
        hit._tip = [s.path, bytes(s.bytes), count(s.files) + " ไฟล์"];
        svg.append(hit);
      });
      return svg;
    });
  }

  // --- Google Drive storage meter -------------------------------------------

  function quota(box, legend, text, q) {
    legend.replaceChildren();
    text.textContent = "";
    text.classList.remove("bad");
    if (q.state !== "ok") return note(box, q.message || "ไม่สามารถดึงข้อมูลพื้นที่ได้");
    const drive = Math.max(0, q.in_drive - q.in_trash), trash = q.in_trash, other = Math.max(0, q.usage - q.in_drive);
    const denom = q.unlimited ? Math.max(q.usage, 1) : Math.max(q.limit, q.usage, 1);
    const free = q.unlimited ? 0 : Math.max(0, q.limit - q.usage);
    const pct = q.unlimited ? 0 : Math.round((q.usage / denom) * 100);
    text.textContent = q.unlimited
      ? `ใช้ไปแล้ว ${bytes(q.usage)} (ไม่จำกัดพื้นที่)`
      : `ใช้ไปแล้ว ${bytes(q.usage)} จาก ${bytes(q.limit)} (${pct}%)` + (q.stale ? " · ข้อมูลเก่า" : "");
    if (pct >= 90) text.classList.add("bad");
    const parts = [["s-acc", "ไฟล์ใน Drive", drive], ["s-upd", "ถังขยะ", trash], ["s-del", "บริการอื่น (Gmail, Photos)", other]];
    if (!q.unlimited) parts.push(["track", "ว่าง", free]);
    for (const [cls, name, v] of parts) {
      const item = html("span", "key");
      const sw = svgEl("svg", { class: "swatch", viewBox: "0 0 10 10", "aria-hidden": "true" });
      sw.append(svgEl("rect", { class: cls === "track" ? "track" : "seg " + cls, width: 10, height: 10, rx: 2 }));
      item.append(sw, `${name} ${bytes(v)}`);
      legend.append(item);
    }
    const aria = text.textContent + `: ไฟล์ใน Drive ${bytes(drive)}, ถังขยะ ${bytes(trash)}, บริการอื่น ${bytes(other)}` +
      (q.unlimited ? "" : `, ว่าง ${bytes(free)}`);
    mount(box, (w) => {
      const svg = svgEl("svg", { class: "chart-svg", viewBox: `0 0 ${w} 22`, role: "img", "aria-label": aria });
      svg.append(svgEl("rect", { class: "track", x: 0, y: 2, width: w, height: 18, rx: 5 }));
      // Segments are clipped to the rounded track.
      const clip = svgEl("clipPath", { id: "quota-clip" });
      clip.append(svgEl("rect", { x: 0, y: 2, width: w, height: 18, rx: 5 }));
      const g = svgEl("g", { "clip-path": "url(#quota-clip)" });
      let x = 0;
      for (const [cls, , v] of parts.slice(0, 3)) {
        const sw = (v / denom) * w;
        if (sw <= 0) continue;
        g.append(svgEl("rect", { class: "seg " + cls, x, y: 2, width: Math.max(sw, 1.5), height: 18 }));
        x += sw;
      }
      svg.append(clip, g);
      return svg;
    });
  }

  // --- live upload speed -----------------------------------------------------

  const speed = {
    samples: [], // bytes/s, newest last
    last: null, // {t, bytes} of the previous poll
    max: 60,
    // push takes the run's cumulative uploaded bytes at poll time.
    push(total) {
      const now = performance.now();
      if (this.last && total >= this.last.bytes && now > this.last.t) {
        this.samples.push(((total - this.last.bytes) / (now - this.last.t)) * 1000);
        if (this.samples.length > this.max) this.samples.shift();
      } else if (this.last && total < this.last.bytes) {
        this.samples = []; // a new run: counters restarted
      }
      this.last = { t: now, bytes: total };
      this.draw();
    },
    reset() {
      this.samples = [];
      this.last = null;
      this.draw();
    },
    draw() {
      const wrap = document.querySelector("[data-chart=speed]");
      if (!wrap) return;
      const box = wrap.querySelector(".plot"), now = wrap.querySelector("[data-speed-now]");
      const s = this.samples;
      wrap.hidden = s.length < 2;
      if (wrap.hidden) return;
      const peak = Math.max(...s);
      now.textContent = `ตอนนี้ ${bytes(s[s.length - 1])}/s · สูงสุด ${bytes(peak)}/s`;
      const aria = `กราฟเส้น: ความเร็วอัปโหลด ${s.length} ตัวอย่างล่าสุด ตอนนี้ ${bytes(s[s.length - 1])}/s สูงสุด ${bytes(peak)}/s`;
      mount(box, (w) => {
        const h = 56, pad = 4, top = peak > 0 ? peak : 1;
        const svg = svgEl("svg", { class: "chart-svg", viewBox: `0 0 ${w} ${h}`, role: "img", "aria-label": aria });
        // Samples fill from the right, so the line grows into a fixed 60-slot window.
        const pts = s.map((v, i) => [((this.max - s.length + i) / (this.max - 1)) * w, h - pad - (v / top) * (h - 2 * pad)]);
        svg.append(svgEl("line", { class: "axis", x1: 0, x2: w, y1: h - pad, y2: h - pad }));
        const line = pts.map((p) => p.map((n) => n.toFixed(1)).join(",")).join(" ");
        svg.append(svgEl("polygon", { class: "area", points: `${pts[0][0]},${h - pad} ${line} ${pts[pts.length - 1][0]},${h - pad}` }));
        svg.append(svgEl("polyline", { class: "line", points: line }));
        return svg;
      });
    },
  };

  // --- the whole section -----------------------------------------------------

  function tile(name, value, sub, title) {
    const b = document.querySelector(`[data-tile=${name}]`);
    if (!b) return;
    b.textContent = value;
    if (sub !== undefined) b.nextElementSibling.textContent = sub;
    if (title) b.parentElement.title = title;
  }

  function render(d) {
    const t = d.tiles;
    tile("files", count(t.files));
    tile("bytes", bytes(t.bytes));
    tile("runs", count(t.runs), t.success_rate === null ? "รอบใน 30 วัน" : `รอบใน 30 วัน · สำเร็จ ${Math.round(t.success_rate * 100)}%`);
    tile("mem", bytes(t.heap_inuse), undefined, "หน่วยความจำที่ขอจากระบบทั้งหมด: " + bytes(t.sys));
    tile("db", bytes(t.db_bytes));

    const plot = (name) => document.querySelector(`[data-chart=${name}] .plot`);
    const part = (name, fn) => { const box = plot(name); if (box) fn(box); };
    part("daily", (b) => daily(b, d.daily));
    part("changes", (b) => changes(b, d.runs));
    part("durations", (b) => durations(b, d.runs));
    part("sources", (b) => sources(b, d.sources));
    part("quota", (b) => {
      const wrap = b.closest("[data-chart]");
      quota(b, wrap.querySelector("[data-quota-legend]"), wrap.querySelector("[data-quota-text]"), d.quota);
    });
  }

  return { render, speed, bytes, duration };
})();
