import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { Script } from "node:vm";
import { parse } from "parse5";

for (const name of ["usage", "quota"]) {
  const path = fileURLToPath(new URL(`../internal/plugin/${name}.html`, import.meta.url));
  const html = await readFile(path, "utf8");
  let count = 0;
  function visit(node) {
    if (node.tagName === "script") {
      if (node.attrs.some(({ name }) => name === "src")) throw new Error(`${path}: external script dependency`);
      const source = (node.childNodes || []).map((item) => item.value || "").join("");
      new Script(source, { filename: `${path}:script-${++count}` });
    }
    if (node.tagName === "link" && node.attrs.some(({ name, value }) => name === "rel" && value === "stylesheet")) {
      throw new Error(`${path}: external stylesheet dependency`);
    }
    for (const child of node.childNodes || []) visit(child);
  }
  visit(parse(html));
  if (!count) throw new Error(`${path}: missing application script`);
  console.log(`${name}.html: inline JavaScript syntax and standalone assets verified`);
}
