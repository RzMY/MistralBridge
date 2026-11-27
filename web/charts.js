(function (root, factory) {
  const charts = factory();
  if (typeof module === "object" && module.exports) module.exports = charts;
  else root.BridgeCharts = charts;
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  "use strict";
  const DAY = 86400000;
  const fields = ["requests", "success", "errors", "input_tokens", "output_tokens", "total_tokens"];
  const dateNumber = value => {
    if (!/^\d{4}-\d{2}-\d{2}$/.test(value || "")) return NaN;
    const n = Date.parse(value + "T00:00:00Z");
    return Number.isFinite(n) && new Date(n).toISOString().slice(0, 10) === value ? n : NaN;
  };
  const dateText = value => new Date(value).toISOString().slice(0, 10);
  const number = value => Number(value || 0).toLocaleString("zh-CN");

  // Aggregate the entire selected interval into at most 60 equal calendar
  // buckets. Missing dates stay identifiable as no records, not known zero use.
  function buckets(daily, options = {}) {
    const rows = daily.map(row => ({ row, time: dateNumber(row.day) })).filter(({ time }) => Number.isFinite(time));
    const from = dateNumber(options.from), to = dateNumber(options.to);
    const filtered = rows.filter(({ time }) => (!Number.isFinite(from) || time >= from) && (!Number.isFinite(to) || time < to));
    if (!filtered.length) return [];
    let start = filtered[0].time, end = start + DAY;
    for (const { time } of filtered) { start = Math.min(start, time); end = Math.max(end, time + DAY); }
    if (Number.isFinite(from)) start = from;
    if (Number.isFinite(to)) end = to;
    if (end <= start) return [];
    const step = Math.max(1, Math.ceil((end - start) / DAY / 60)) * DAY;
    const result = Array.from({ length: Math.ceil((end - start) / step) }, (_, i) => ({
      from: dateText(start + i * step), to: dateText(Math.min(end, start + (i + 1) * step) - DAY),
      samples: 0, ...Object.fromEntries(fields.map(field => [field, 0]))
    }));
    for (const { row, time } of filtered) {
      const bucket = result[Math.floor((time - start) / step)];
      bucket.samples++;
      for (const field of fields) {
        const value = Number(row[field]);
        if (Number.isFinite(value) && value > 0) bucket[field] += value;
      }
    }
    return result;
  }

  function range(selected, now = new Date(), customFrom = "", customTo = "") {
    const query = new URLSearchParams({ timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC" });
    if (selected === "all") return query;
    let from = new Date(now.getFullYear(), now.getMonth(), now.getDate());
    let to = new Date(from); to.setDate(to.getDate() + 1);
    if (selected === "yesterday") { to = new Date(from); from.setDate(from.getDate() - 1); }
    else if (selected === "7d" || selected === "30d") from.setDate(from.getDate() - (selected === "7d" ? 6 : 29));
    else if (selected === "custom") {
      from = new Date(customFrom); to = new Date(customTo);
      if (!customFrom || !customTo || !Number.isFinite(+from) || !Number.isFinite(+to) || from >= to) throw new Error("请选择有效的起止时间");
    } else if (selected !== "today") throw new Error("无效的日期范围");
    query.set("from", from.toISOString()); query.set("to", to.toISOString());
    query.set("granularity", "auto");
    return query;
  }

  function seriesBuckets(series, options) {
    if (options.granularity !== "hour") return buckets(series.map(row => ({ ...row, day: row.label })));
    const clock = value => new Intl.DateTimeFormat("zh-CN", { timeZone: options.timezone, hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).format(new Date(value));
    return series.map(row => ({ ...row, from: clock(row.start), to: clock(row.end), samples: row.requests, detailLabel: row.label }));
  }

  function detailRows(series, serverTime) {
    const cutoff = Date.parse(serverTime);
    if (!Number.isFinite(cutoff)) return [];
    return series.filter(row => Number(row.requests) > 0 && Date.parse(row.start) <= cutoff)
      .sort((a, b) => Date.parse(b.start) - Date.parse(a.start));
  }

  function render(container, daily, options = {}) {
    container.replaceChildren();
    const data = options.series ? seriesBuckets(daily, options) : buckets(daily, options);
    if (!data.length) {
      const empty = document.createElement("div");
      empty.className = "chart-empty"; empty.textContent = "暂无可绘制的日期数据";
      container.append(empty); return;
    }
    const requests = options.metric === "requests";
    const series = requests ? ["requests"] : ["total_tokens"];
    const classes = requests ? ["chart-success"] : ["chart-input"];
    const label = requests ? "请求趋势" : options.granularity === "hour" ? "分时 Token" : "Token 用量";
    const width = 640, height = 240, left = 58, right = 12, top = 18, bottom = 36;
    const plotWidth = width - left - right, plotHeight = height - top - bottom;
    const max = Math.max(1, ...data.map(row => row[series[0]]));
    const unit = Math.max(1, 10 ** Math.floor(Math.log10(max / 4)));
    const tick = [1, 2, 5, 10].find(step => step * unit >= max / 4) * unit;
    const ceiling = tick * 4;
    function svgNode(tag, attributes = {}, text) {
      const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
      for (const [key, value] of Object.entries(attributes)) node.setAttribute(key, String(value));
      if (text != null) node.textContent = text;
      return node;
    }
    const svg = svgNode("svg", { viewBox: `0 0 ${width} ${height}`, "aria-label": `${label}，${data[0].from} 至 ${data[data.length - 1].to}（${options.timezone || "UTC"}）` });
    const short = value => value >= 1e8 ? `${+(value / 1e8).toFixed(1)}亿` : value >= 1e4 ? `${+(value / 1e4).toFixed(1)}万` : `${+value.toFixed(1)}`;
    for (let i = 0; i <= 4; i++) {
      const y = top + plotHeight * i / 4;
      svg.append(svgNode("line", { x1: left, y1: y, x2: width - right, y2: y, class: "chart-grid" }));
      svg.append(svgNode("text", { x: left - 10, y: y + 4, "text-anchor": "end", class: "chart-axis" }, short(ceiling * (4 - i) / 4)));
    }
    const tooltip = document.createElement("div");
    tooltip.className = "chart-tooltip"; tooltip.hidden = true;
    const slot = plotWidth / data.length, barWidth = Math.min(38, slot * .72);
    const labelIndices = new Set([0, Math.floor((data.length - 1) / 2), data.length - 1]);
    for (let i = 0; i < data.length; i++) {
      const row = data[i], x = left + slot * (i + .5);
      const date = row.from === row.to ? row.from : `${row.from} 至 ${row.to}`;
      const detail = `${row.detailLabel || date}\n${requests ? "总请求数" : "总 Token"} ${number(row[series[0]])}`;
      const group = svgNode("g", { tabindex: "0", role: "img", "aria-label": detail, class: "chart-column" });
      group.append(svgNode("rect", { x: x - slot / 2, y: top, width: slot, height: plotHeight, class: "chart-hit" }));
      let y = top + plotHeight;
      series.forEach((field, index) => {
        const h = row[field] / ceiling * plotHeight;
        y -= h;
        group.append(svgNode("rect", { x: x - barWidth / 2, y, width: barWidth, height: h, rx: 2, class: classes[index] }));
      });
      const show = () => {
        tooltip.textContent = detail; tooltip.hidden = false;
        const ratio = x / width;
        tooltip.style.left = `${Math.max(5, Math.min(95, ratio * 100))}%`;
        tooltip.style.transform = `translateX(${ratio < .3 ? "0" : ratio > .7 ? "-100%" : "-50%"})`;
      };
      group.addEventListener("pointerenter", show);
      group.addEventListener("focus", show);
      group.addEventListener("pointerleave", () => { tooltip.hidden = true; });
      group.addEventListener("blur", () => { tooltip.hidden = true; });
      group.addEventListener("keydown", event => { if (event.key === "Escape") tooltip.hidden = true; });
      svg.append(group);
      if (labelIndices.has(i)) svg.append(svgNode("text", { x, y: height - 10, "text-anchor": data.length === 1 ? "middle" : i === 0 ? "start" : i === data.length - 1 ? "end" : "middle", class: "chart-axis" }, row.from));
    }
    container.append(svg, tooltip);
  }
  return { buckets, range, seriesBuckets, detailRows, render };
});
