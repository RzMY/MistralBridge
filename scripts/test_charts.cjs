const assert = require("node:assert/strict");
const { test } = require("node:test");
const { buckets, range, seriesBuckets, detailRows } = require("../web/charts.js");

test("calendar gaps and exclusive date boundaries remain visible", () => {
  const result = buckets([
    { day: "2026-11-28", requests: 7, success: 6, errors: 1 },
    { day: "2026-11-26", requests: 3, success: 3 },
    { day: "2026-11-29", requests: 99 }
  ], { from: "2026-11-26", to: "2026-11-29" });
  assert.deepEqual(result.map(row => [row.from, row.requests, row.samples]), [
    ["2026-11-26", 3, 1], ["2026-11-27", 0, 0], ["2026-11-28", 7, 1]
  ]);
});

test("long-range charts preserve every count and token without truncation", () => {
  const rows = Array.from({ length: 1200 }, (_, i) => ({
    day: new Date(Date.UTC(2022, 0, 1 + i)).toISOString().slice(0, 10),
    requests: 3, success: 2, errors: 1, input_tokens: i, output_tokens: 9, total_tokens: i + 9
  }));
  const result = buckets(rows.reverse());
  assert.ok(result.length <= 60);
  for (const field of ["requests", "success", "errors", "input_tokens", "output_tokens", "total_tokens"]) {
    assert.equal(result.reduce((sum, row) => sum + row[field], 0), rows.reduce((sum, row) => sum + row[field], 0));
  }
  assert.equal(result[0].from, "2022-01-01");
  assert.equal(result.at(-1).to, rows[0].day);
});

test("unknown and malformed dates never invent a dated trend", () => {
  assert.deepEqual(buckets([{ day: "unknown", requests: 10 }, { day: "2026-02-30", requests: 1 }]), []);
  assert.deepEqual(buckets([]), []);
  assert.deepEqual(buckets([{ day: "2026-11-28", requests: 1 }], { from: "2026-12-01" }), []);
});

test("one day, zero tokens and invalid values produce finite chart data", () => {
  const [row] = buckets([{ day: "2026-11-28", requests: 1, errors: 1, input_tokens: 0, output_tokens: null, total_tokens: NaN }]);
  assert.equal(row.from, row.to);
  assert.equal(row.requests, 1);
  assert.equal(row.total_tokens, 0);
  assert.equal(row.output_tokens, 0);
});

test("today and yesterday follow the browser system calendar, including DST", () => {
  const { execFileSync } = require("node:child_process");
  const script = `const {range}=require('./web/charts.js');const now=new Date(process.argv[1]);console.log(JSON.stringify(['today','yesterday'].map(s=>Object.fromEntries(range(s,now)))));`;
  for (const [zone, now, expected, hours] of [
    ["Asia/Singapore", "2026-11-27T17:00:00Z", "2026-11-27T16:00:00.000Z", 24],
    ["America/New_York", "2026-03-08T15:00:00Z", "2026-03-08T05:00:00.000Z", 23],
    ["America/New_York", "2026-11-01T15:00:00Z", "2026-11-01T04:00:00.000Z", 25]
  ]) {
    const [today,yesterday] = JSON.parse(execFileSync(process.execPath,["-e",script,now],{cwd:require("node:path").join(__dirname,".."),env:{...process.env,TZ:zone},encoding:"utf8"}));
    assert.equal(today.from, expected);
    assert.equal((Date.parse(today.to)-Date.parse(today.from))/3600000,hours);
    assert.equal(yesterday.to,today.from);
  }
});

test("hourly charts use total tokens, including returned totals different from input plus output", () => {
  const [row] = seriesBuckets([{start:"2026-11-27T16:00:00Z",end:"2026-11-27T17:00:00Z",label:"11-28 00:00 +08:00",requests:2,total_tokens:900,input_tokens:100,output_tokens:200}], {granularity:"hour",timezone:"Asia/Singapore"});
  assert.equal(row.from,"00:00"); assert.equal(row.to,"01:00"); assert.equal(row.total_tokens,900);
  assert.throws(()=>range("custom",new Date(),"2026-11-28T12:00","2026-11-28T11:00"));
});

test("detail rows exclude empty and future periods using server time", () => {
  const rows = [
    {start:"2026-11-28T00:00:00Z",requests:2},
    {start:"2026-11-28T01:00:00Z",requests:0},
    {start:"2026-11-28T02:00:00Z",requests:1},
    {start:"2026-11-28T03:00:00Z",requests:4}
  ];
  assert.deepEqual(detailRows(rows,"2026-11-28T02:30:00Z"),[rows[2],rows[0]]);
  assert.deepEqual(detailRows(rows,"invalid"),[]);
  assert.equal(rows[0].start,"2026-11-28T00:00:00Z");
  const all = range("all");
  assert.equal(all.has("from"),false); assert.equal(all.has("to"),false);
});
