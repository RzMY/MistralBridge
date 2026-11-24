const assert = require("node:assert/strict");
const { test } = require("node:test");
const json = require("../web/json.js");

test("formatting retains exact number literals and property order", () => {
  const source = '{"seed":9007199254740993,"negative":-9223372036854775808,"exponent":1.2300e+42,"decimal":0.10000000000000001,"signedZero":-0,"nested":[true,null,{},[]]}';
  const formatted = json.format(source);
  assert.equal(json.compact(formatted), source);
  assert.ok(formatted.includes('  "seed": 9007199254740993,\n'));
});

test("escaped strings, Unicode, duplicate keys and prototype names are preserved", () => {
  const source = '{"__proto__":{"constructor":"literal"},"x":1,"x":2,"quoted":"a\\\"b\\nc","unicode":"中文\\u003cscript\\u003e"}';
  assert.equal(json.compact(json.format(source)), source);
  assert.equal(json.parse(source).entries.length, 5);
  assert.equal({}.constructor, Object);
});

test("round trips nested JSON values, empty containers and primitive roots", () => {
  for (const value of [{ messages: [{ role: "user", content: [{ type: "text", text: "你好\n}" }] }], headers: { "X-Test": ["one", "two"] } }, [], {}, null, true, "text", 42]) {
    const source = JSON.stringify(value);
    assert.deepEqual(JSON.parse(json.format(source)), value);
    assert.equal(json.compact(source), source);
  }
});

test("invalid JSON fails before it can replace valid preview input", () => {
  for (const source of ["", "undefined", "{", "[1,]", '{"x":01}', '"bad\nstring"', "{} {}"])
    assert.throws(() => json.format(source), SyntaxError);
  assert.throws(() => json.format("[".repeat(130) + "0" + "]".repeat(130)), /层级/);
});
