// Keep numeric tokens as text: JSON.parse + JSON.stringify would round int64s.
const BridgeJSON = (() => {
  function parse(text) {
    if (typeof text !== "string") throw new TypeError("JSON 输入必须是文本");
    JSON.parse(text); // Validate complete JSON grammar before the token walk.
    let position = 0;
    let nodes = 0;
    const stringToken = /"(?:\\[\s\S]|[^"\\])*"/y;
    const primitiveToken = /-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?|true|false|null/y;
    function whitespace() {
      while (/[ \t\r\n]/.test(text[position] || "x")) position++;
    }
    function quoted() {
      stringToken.lastIndex = position;
      const token = stringToken.exec(text)[0];
      position += token.length;
      return token;
    }
    function value(depth) {
      if (depth > 128 || ++nodes > 250000) throw new Error("JSON 层级或字段过多，请缩小预览范围");
      whitespace();
      const character = text[position];
      if (character === "{" || character === "[") {
        position++;
        const type = character === "{" ? "object" : "array";
        const close = type === "object" ? "}" : "]";
        const entries = [];
        whitespace();
        while (text[position] !== close) {
          let key;
          if (type === "object") {
            key = quoted();
            whitespace(); position++; // colon
          }
          entries.push({ key, value: value(depth + 1) });
          whitespace();
          if (text[position] === ",") { position++; whitespace(); }
        }
        position++;
        return { type, entries };
      }
      if (character === '"') return { type: "string", raw: quoted() };
      primitiveToken.lastIndex = position;
      const raw = primitiveToken.exec(text)[0];
      position += raw.length;
      return { type: raw === "null" ? "null" : raw === "true" || raw === "false" ? "boolean" : "number", raw };
    }
    return value(0);
  }

  function stringify(node, indentation, depth = 0) {
    if (!node.entries) return node.raw;
    const object = node.type === "object";
    const open = object ? "{" : "[", close = object ? "}" : "]";
    if (!node.entries.length) return open + close;
    const space = indentation ? " " : "";
    const newline = indentation ? "\n" : "";
    const pad = indentation.repeat(depth + 1);
    const fields = node.entries.map(entry => pad + (object ? entry.key + ":" + space : "") + stringify(entry.value, indentation, depth + 1));
    return open + newline + fields.join("," + newline) + newline + indentation.repeat(depth) + close;
  }

  function format(text) { return stringify(parse(text), "  "); }
  function compact(text) { return stringify(parse(text), ""); }

  function mount(container, text) {
    const root = parse(text);
    const document = container.ownerDocument;
    const branches = new Map();
    const span = (className, content) => {
      const element = document.createElement("span");
      element.className = className; element.textContent = content;
      return element;
    };
    function build(node, key, index, depth, comma) {
      const object = node.type === "object";
      const label = key !== undefined ? key + ": " : index !== undefined ? String(index) + ": " : "";
      const prefix = span(key !== undefined ? "json-key" : "json-index", label);
      if (!node.entries || !node.entries.length) {
        const line = document.createElement("div");
        line.className = "json-leaf";
        line.append(prefix, span("json-" + node.type, node.entries ? object ? "{}" : "[]" : node.raw), span("json-punctuation", comma));
        return line;
      }
      const details = document.createElement("details");
      details.className = "json-branch";
      const summary = document.createElement("summary");
      const open = object ? "{" : "[", close = object ? "}" : "]";
      summary.append(prefix, span("json-punctuation", open), span("json-collapsed", ` … ${node.entries.length} ${object ? "个字段" : "项"} ${close}${comma}`));
      const children = document.createElement("div");
      children.className = "json-children";
      const closing = document.createElement("div");
      closing.className = "json-close"; closing.textContent = close + comma;
      details.append(summary, children, closing);
      let loaded = false;
      const load = () => {
        if (loaded) return;
        loaded = true;
        const fragment = document.createDocumentFragment();
        node.entries.forEach((entry, i) => fragment.append(build(entry.value, entry.key, object ? undefined : i, depth + 1, i < node.entries.length - 1 ? "," : "")));
        children.append(fragment);
      };
      branches.set(details, load);
      details.addEventListener("toggle", () => { if (details.open) load(); });
      if (depth < 2) { load(); details.open = true; }
      return details;
    }
    container.replaceChildren(build(root, undefined, undefined, 0, ""));
    return {
      expandAll() {
        // Map iteration visits descendants added by each lazy loader.
        for (const [details, load] of branches) { load(); details.open = true; }
      },
      collapseAll() { for (const details of branches.keys()) details.open = false; }
    };
  }

  return { parse, format, compact, mount };
})();
if (typeof module !== "undefined" && module.exports) module.exports = BridgeJSON;
