// The walking skeleton (stage S2b): one sandbox payment and the server's
// live trace, run from a real browser to prove the hosting, CORS,
// server-sent events and HTTP/2. The proof console replaces it in S7.
"use strict";

// ?api= points the page at another server, such as a local `just demo`.
const API = (new URLSearchParams(location.search).get("api") ||
  document.querySelector('meta[name="ledgerline-api"]').content).replace(/\/+$/, "");

const $ = (id) => document.getElementById(id);
let session = null;
let stream = null;
let events = 0;

function say(text, isError = false) {
  $("status").textContent = text;
  $("status").classList.toggle("error", isError);
}

async function call(method, path, body, key) {
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (key) headers["Authorization"] = "Bearer " + key;
  if (method === "POST") headers["Idempotency-Key"] = crypto.randomUUID();
  const res = await fetch(API + path, {
    method, headers, cache: "no-store",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let json = null;
  try { json = await res.json(); } catch { /* not JSON: reported by status */ }
  return { status: res.status, json, retryAfter: res.headers.get("Retry-After") };
}

function failed(r) {
  const e = r.json && r.json.error;
  say(e ? `${r.status} ${e.code}: ${e.message}` : `The API answered ${r.status}.`, true);
}

function unreachable(err) {
  say(`The API could not be reached (${err.message}). If the server is awake, its CORS settings may not list this page's origin, ${location.origin}.`, true);
}

// nextHopProtocol is h3 or h2 when requests share one multiplexed
// connection, and http/1.1 when the browser caps them at six.
function showProtocol() {
  const seen = new Set(performance.getEntriesByType("resource")
    .filter((e) => e.name.startsWith(API) && e.nextHopProtocol)
    .map((e) => e.nextHopProtocol));
  if (seen.size) $("protocol").textContent = [...seen].join(", ");
}

async function checkHealth() {
  $("api").textContent = API;
  say("Reaching the API. A free server that was asleep can take a minute to wake.");
  const started = performance.now();
  try {
    const r = await call("GET", "/healthz");
    const ms = Math.round(performance.now() - started);
    if (r.status !== 200 || !r.json) return failed(r);
    $("health").textContent = `${r.json.status}, commit ${r.json.commit.slice(0, 7)}, ${ms} ms`;
    $("start").disabled = false;
    $("burst").disabled = false;
    say("");
  } catch (err) {
    $("health").textContent = "unreachable";
    unreachable(err);
  }
  showProtocol();
}

async function startSession() {
  $("start").disabled = true;
  try {
    const r = await call("POST", "/demo/sessions", {});
    if (r.status !== 200) return failed(r);
    session = r.json;
    const ends = new Date(session.expires_at * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    $("session").textContent = `${session.id}, ends at ${ends}`;
    $("trace").replaceChildren();
    events = 0;
    openStream();
    $("pay").disabled = false;
    say("Session started. Its trace is streaming below.");
  } catch (err) {
    unreachable(err);
  } finally {
    $("start").disabled = false;
    showProtocol();
  }
}

function openStream() {
  if (stream) stream.close();
  stream = new EventSource(API + session.stream_url);
  $("stream").textContent = "connecting";
  stream.onopen = () => { $("stream").textContent = "open"; };
  stream.onerror = () => {
    $("stream").textContent = stream.readyState === EventSource.CLOSED ? "closed" : "reconnecting";
  };
  stream.onmessage = (m) => append(JSON.parse(m.data));
  stream.addEventListener("expired", () => {
    stream.close();
    $("stream").textContent = "closed, the session ended";
    $("pay").disabled = true;
    say("The session ended after its hour. Start another to keep going.");
  });
}

function append(e) {
  const list = $("trace");
  const atBottom = list.scrollHeight - list.scrollTop - list.clientHeight < 24;

  const item = document.createElement("li");
  const head = document.createElement("div");
  head.className = "head";
  for (const [cls, text] of [["seq", String(e.seq)], ["lane", e.lane], ["site", e.site]]) {
    const span = document.createElement("span");
    span.className = cls;
    span.textContent = text;
    head.append(span);
  }
  item.append(head);
  const detail = { ...(e.payment_id ? { payment_id: e.payment_id } : {}), ...(e.data || {}) };
  if (Object.keys(detail).length) {
    const code = document.createElement("code");
    code.textContent = JSON.stringify(detail);
    item.append(code);
  }
  list.append(item);

  events++;
  $("empty").hidden = true;
  $("count").textContent = `${events} event${events === 1 ? "" : "s"}`;
  if (atBottom) list.scrollTop = list.scrollHeight;
}

async function pay() {
  const card = $("card").value;
  $("pay").disabled = true;
  try {
    say("Creating a payment intent for ₹499.00.");
    let r = await call("POST", "/v1/payment_intents", { amount: 49900, currency: "inr" }, session.secret_key);
    if (r.status !== 200) return failed(r);
    const id = r.json.id;
    say(`Confirming ${id} with ${card}.`);
    r = await call("POST", `/v1/payment_intents/${id}/confirm`, { payment_method: card }, session.secret_key);
    if (r.status !== 200) return failed(r);
    say(`${id} is ${r.json.status}.`);
  } catch (err) {
    unreachable(err);
  } finally {
    $("pay").disabled = !session || !stream || stream.readyState === EventSource.CLOSED;
    showProtocol();
  }
}

// Twenty requests fired together. Over HTTP/1.1 a browser sends at most
// six at a time to one origin; over h2 or h3 all twenty go at once. The
// retry storm in the console depends on the second.
async function burst() {
  const n = 20;
  $("burst").disabled = true;
  say(`Sending ${n} requests at once.`);
  const started = performance.now();
  try {
    // A request's timing is recorded once its body has been read.
    const statuses = await Promise.all(Array.from({ length: n }, () =>
      fetch(API + "/healthz", { cache: "no-store" }).then(async (r) => { await r.arrayBuffer(); return r.status; })));
    const ms = Math.round(performance.now() - started);
    const ok = statuses.filter((s) => s === 200).length;
    await new Promise((resolve) => setTimeout(resolve)); // timings are queued, not synchronous
    const entries = performance.getEntriesByType("resource")
      .filter((e) => e.name === API + "/healthz" && e.startTime >= started);
    const protocols = [...new Set(entries.map((e) => e.nextHopProtocol).filter(Boolean))].join(", ") || "an unreported protocol";
    say(`${ok} of ${n} answered in ${ms} ms over ${protocols}, with up to ${peakInFlight(entries)} in flight at once.`);
  } catch (err) {
    unreachable(err);
  } finally {
    $("burst").disabled = false;
    showProtocol();
  }
}

// peakInFlight is the most requests on the wire together, from when each
// was sent (not queued) to when its answer finished.
function peakInFlight(entries) {
  const edges = entries.flatMap((e) => [[e.requestStart || e.startTime, 1], [e.responseEnd, -1]]);
  edges.sort((a, b) => a[0] - b[0] || a[1] - b[1]);
  let now = 0;
  let peak = 0;
  for (const [, step] of edges) {
    now += step;
    peak = Math.max(peak, now);
  }
  return peak;
}

$("start").addEventListener("click", startSession);
$("pay").addEventListener("click", pay);
$("burst").addEventListener("click", burst);
checkHealth();
