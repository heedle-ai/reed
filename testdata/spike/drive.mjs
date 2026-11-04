// Spike (roadmap 9.5): drive a two-page form through Playwright MCP over
// stdio, by hand. node drive.mjs <chromium> <@playwright/mcp cli.js>
import http from "node:http";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";
const exe = process.argv[2], cli = process.argv[3];
const got = [], hits = [];
const page = (title, body) => `<!doctype html><title>${title}</title><h1>${title}</h1>${body}`;
const srv = http.createServer((q, r) => {
  hits.push(q.method + " " + q.url);
  let b = ""; q.on("data", (c) => (b += c)); q.on("end", () => {
    if (q.url === "/volunteer") r.end(page("Volunteer sign-up: you", `<form method=post action=/volunteer/2><label>Name <input name=name required></label><label>Email <input name=email type=email required></label><button>Next</button></form><img src="http://decoy.example.org/pixel.png">`));
    else if (q.url === "/volunteer/2") { got.push(b); r.end(page("Volunteer sign-up: shift", `<form method=post action=/volunteer/done><input type=hidden name=prev value="${encodeURIComponent(b)}"><label>Shift <select name=shift><option>Friday</option><option>Saturday</option><option>Sunday</option></select></label><button>Submit</button></form>`)); }
    else if (q.url === "/volunteer/done") { got.push(b); r.end(page("Thank you", "<p>You're signed up.</p>")); }
    else { r.statusCode = 404; r.end("no"); }
  });
}).listen(0, "127.0.0.1");
await new Promise((r) => srv.once("listening", r));
const origin = `http://127.0.0.1:${srv.address().port}`;
const args = [cli, "--headless", "--isolated", "--executable-path", exe, "--allowed-origins", origin, "--no-webmcp", "--image-responses", "omit"];
const transport = new StdioClientTransport({ command: process.execPath, args, stderr: "inherit", env: { ...process.env } });
const c = new Client({ name: "spike", version: "0" });
const t0 = Date.now();
await c.connect(transport);
const tools = (await c.listTools()).tools.map((t) => t.name);
console.log("server", JSON.stringify(c.getServerVersion()), "tools", tools.length, tools.join(" "));
const call = async (name, a) => { const r = await c.callTool({ name, arguments: a }); const text = r.content.filter((x) => x.type === "text").map((x) => x.text).join("\n"); console.log(`\n== ${name} ${JSON.stringify(a)} ${r.isError ? "ERROR" : ""}\n` + text.slice(0, 900)); return text; };
const ref = (snap, label) => { const m = new RegExp(`${label}[^\\n]*\\[ref=([a-z0-9]+)\\]`).exec(snap); if (!m) throw new Error("no ref for " + label); return m[1]; };
await call("browser_navigate", { url: origin + "/volunteer" });
let s = await call("browser_snapshot", {});
await call("browser_fill_form", { fields: [
  { name: "Name", type: "textbox", target: ref(s, 'textbox "Name'), value: "Ada Lovelace" },
  { name: "Email", type: "textbox", target: ref(s, 'textbox "Email'), value: "ada@example.org" }] });
await call("browser_click", { element: "Next", target: ref(s, 'button "Next"') });
s = await call("browser_snapshot", {});
await call("browser_select_option", { element: "Shift", target: ref(s, 'combobox "Shift'), values: ["Saturday"] });
await call("browser_click", { element: "Submit", target: ref(s, 'button "Submit"') });
await call("browser_snapshot", {});
await call("browser_navigate", { url: "http://decoy.example.org/" });
await call("browser_evaluate", { function: "() => document.title" });
console.log("\nform received:", JSON.stringify(got), "\nserver hits:", JSON.stringify(hits), "\nseconds:", (Date.now() - t0) / 1000);
await c.close(); srv.close();
