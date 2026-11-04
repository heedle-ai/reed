// Spike round 2 (roadmap 9.5): the same two-page form, driven through
// Playwright MCP over stdio, with reed's allowlist switches in force.
//
//   node drive2.mjs <launch|cdp> <chromium> <@playwright/mcp cli.js> <pw-allow yes|no> <decoy http://IP:PORT>
//
// launch: Playwright MCP starts Chromium itself (--executable-path), with
//         reed's switches passed as launch arguments (--config).
// cdp:    Chromium is started here with reed's switches, as reed starts it,
//         and Playwright MCP attaches to it (--cdp-endpoint).
// The form is served on this machine's non-loopback address, since reed's
// allowlist refuses loopback. The decoy is a server elsewhere that counts
// what reaches it; the page asks for it by image, and the script navigates.
import http from "node:http";
import os from "node:os";
import fs from "node:fs";
import { spawn } from "node:child_process";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";
const [mode, exe, cli, pwAllow, decoy] = process.argv.slice(2);
const ip = Object.values(os.networkInterfaces()).flat().find((a) => a.family === "IPv4" && !a.internal).address;
const got = [], hits = [];
const page = (title, body) => `<!doctype html><title>${title}</title><h1>${title}</h1>${body}`;
const srv = http.createServer((q, r) => {
  hits.push(q.method + " " + q.url);
  let b = ""; q.on("data", (c) => (b += c)); q.on("end", () => {
    if (q.url === "/volunteer") r.end(page("Volunteer sign-up: you", `<form method=post action=/volunteer/2><label>Name <input name=name required></label><label>Email <input name=email type=email required></label><button>Next</button></form><img src="${decoy}/pixel.png"><img src="http://decoy.example.org/pixel.png">`));
    else if (q.url === "/volunteer/2") { got.push(b); r.end(page("Volunteer sign-up: shift", `<form method=post action=/volunteer/done><label>Shift <select name=shift><option>Friday</option><option>Saturday</option><option>Sunday</option></select></label><button>Submit</button></form>`)); }
    else if (q.url === "/volunteer/done") { got.push(b); r.end(page("Thank you", "<p>You're signed up.</p>")); }
    else { r.statusCode = 404; r.end("no"); }
  });
}).listen(8080, "0.0.0.0");
await new Promise((r) => srv.once("listening", r));
const origin = `http://${ip}:8080`;
// reed's ChromiumFlags for the allowlist [ip] (reed.go), without the
// launch-only ones Playwright sets itself.
const reedFlags = ["--no-sandbox", "--disable-dev-shm-usage", "--no-first-run", "--no-default-browser-check",
  "--disable-background-networking", "--disable-component-update", "--disable-sync",
  "--proxy-server=http://127.0.0.1:9", `--proxy-bypass-list=<-loopback>;${ip}`,
  `--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE ${ip}`, "--force-webrtc-ip-handling-policy=disable_non_proxied_udp"];
let chromium, args = [cli, "--no-webmcp", "--image-responses", "omit", "--output-dir", "/tmp/pwout"];
if (pwAllow === "yes") args.push("--allowed-origins", origin);
if (mode === "cdp") {
  chromium = spawn(exe, ["--headless", "--user-data-dir=/tmp/profile", "--remote-debugging-port=9222", ...reedFlags, "about:blank"], { stdio: "ignore" });
  for (let i = 0; i < 100; i++) { try { await (await fetch("http://127.0.0.1:9222/json/version")).json(); break; } catch { await new Promise((r) => setTimeout(r, 100)); } }
  args.push("--cdp-endpoint", "http://127.0.0.1:9222");
} else {
  fs.writeFileSync("/tmp/pwconfig.json", JSON.stringify({ browser: { launchOptions: { args: reedFlags }, contextOptions: { acceptDownloads: false } } }));
  args.push("--headless", "--isolated", "--executable-path", exe, "--config", "/tmp/pwconfig.json");
}
const out = { mode, pwAllow, chromium: "", form: null, formHits: null, decoyNav: "", evaluate: "", argsHaveProxy: false, seconds: 0, error: "" };
const t0 = Date.now();
const c = new Client({ name: "spike", version: "0" });
try {
  await c.connect(new StdioClientTransport({ command: process.execPath, args, stderr: "inherit", env: { ...process.env } }));
  const call = async (name, a) => { const r = await c.callTool({ name, arguments: a }); return { err: !!r.isError, text: r.content.filter((x) => x.type === "text").map((x) => x.text).join("\n") }; };
  const ref = (s, label) => { const m = new RegExp(`${label}[^\\n]*\\[ref=([a-z0-9]+)\\]`).exec(s); if (!m) throw new Error("no ref for " + label + " in " + s.slice(0, 400)); return m[1]; };
  await call("browser_navigate", { url: origin + "/volunteer" });
  let s = (await call("browser_snapshot", {})).text;
  await call("browser_fill_form", { fields: [
    { name: "Name", type: "textbox", target: ref(s, 'textbox "Name'), value: "Ada Lovelace" },
    { name: "Email", type: "textbox", target: ref(s, 'textbox "Email'), value: "ada@example.org" }] });
  await call("browser_click", { element: "Next", target: ref(s, 'button "Next"') });
  s = (await call("browser_snapshot", {})).text;
  await call("browser_select_option", { element: "Shift", target: ref(s, 'combobox "Shift'), values: ["Saturday"] });
  await call("browser_click", { element: "Submit", target: ref(s, 'button "Submit"') });
  out.decoyNav = (await call("browser_navigate", { url: decoy + "/nav" })).text.split("\n").find((l) => /Error|URL/.test(l)) || "";
  out.decoyNav += " | " + ((await call("browser_navigate", { url: "http://decoy.example.org/" })).text.split("\n").find((l) => /Error|URL/.test(l)) || "");
  out.evaluate = (await call("browser_evaluate", { function: "() => navigator.userAgent" })).text.split("\n").slice(0, 3).join(" ");
  await new Promise((r) => setTimeout(r, 1000));
  // The Chromium that actually ran, and its switches.
  for (const p of fs.readdirSync("/proc").filter((d) => /^\d+$/.test(d))) {
    try {
      const a = fs.readFileSync(`/proc/${p}/cmdline`, "utf8").split("\0");
      if (/chromium$/.test(a[0]) && !a.some((x) => x.startsWith("--type="))) { out.argsHaveProxy = a.includes("--proxy-server=http://127.0.0.1:9") && a.some((x) => x.startsWith("--host-resolver-rules=")); out.chromiumArgs = a.length; }
    } catch {}
  }
  await c.close();
} catch (e) { out.error = String(e).slice(0, 400); }
out.form = got; out.formHits = hits; out.seconds = (Date.now() - t0) / 1000;
console.log("RESULT " + JSON.stringify(out));
if (chromium) chromium.kill();
srv.close();
process.exit(0);
