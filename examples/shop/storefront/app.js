// Beans & Leaves — a tiny storefront for the DCMS shop demo. Vanilla JS, no build.
// Everything it does is a plain call to the DCMS REST API on the same origin:
//   products      GET  /api/v1/products?expand=image&limit&cursor
//   product       GET  /api/v1/products/:id?expand=image
//   sign up        POST /api/v1/customers            (public)
//   place order    POST /api/v1/orders               (public; the checkout hook
//                                                      prices it, checks stock, "charges")
// Identity + cart live in localStorage — this is a storefront, not the admin.

const API = "/api/v1";
const PAGE = 8;

// ── tiny helpers ───────────────────────────────────────────────────────────────
const $ = (sel, el = document) => el.querySelector(sel);
const el = (html) => { const t = document.createElement("template"); t.innerHTML = html.trim(); return t.content.firstElementChild; };
const money = (v) => "$" + Number(v).toFixed(2);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

async function api(path, opts = {}) {
  const res = await fetch(API + path, {
    headers: { "Content-Type": "application/json", ...(opts.headers || {}) },
    ...opts,
  });
  const body = res.status === 204 ? {} : await res.json().catch(() => ({}));
  if (!res.ok) {
    const e = body.error || {};
    throw Object.assign(new Error(e.message || res.statusText), { code: e.code, status: res.status });
  }
  return body;
}

// ── persisted state ──────────────────────────────────────────────────────────
const store = {
  get cart() { try { return JSON.parse(localStorage.getItem("bl_cart")) || []; } catch { return []; } },
  set cart(v) { localStorage.setItem("bl_cart", JSON.stringify(v)); renderCartCount(); },
  get customer() { try { return JSON.parse(localStorage.getItem("bl_customer")); } catch { return null; } },
  set customer(v) { v ? localStorage.setItem("bl_customer", JSON.stringify(v)) : localStorage.removeItem("bl_customer"); renderAccount(); },
  get orders() { try { return JSON.parse(localStorage.getItem("bl_orders")) || []; } catch { return []; } },
  set orders(v) { localStorage.setItem("bl_orders", JSON.stringify(v)); },
};

function addToCart(p, qty = 1) {
  const cart = store.cart;
  const line = cart.find((l) => l.id === p.id);
  if (line) line.qty += qty;
  else cart.push({ id: p.id, name: p.name, price: p.price, image: imgUrl(p), qty });
  store.cart = cart;
  toast(`Added ${p.name}`);
}
// A product tile: a coloured gradient with the product's initial, overlaid by the
// real image once it loads (and only if it isn't a 1x1 placeholder). No CSP here.
const PALETTE = [["#6f4e37", "#a67c52"], ["#3a6b35", "#7bA05b"], ["#8a5a44", "#c08457"], ["#4a5568", "#718096"], ["#7b341e", "#c05621"]];
function grad(name) {
  let h = 0; for (const ch of String(name)) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  const [a, b] = PALETTE[h % PALETTE.length];
  return `linear-gradient(135deg, ${a}, ${b})`;
}
function tile(name, url, cls) {
  const initial = esc((String(name).trim()[0] || "?").toUpperCase());
  const img = url ? `<img alt="${esc(name)}" src="${esc(url)}" onerror="this.remove()" onload="if(this.naturalWidth>2)this.classList.add('shown')">` : "";
  return `<div class="tile ${cls}" style="background:${grad(name)}"><span class="ph">${initial}</span>${img}</div>`;
}
const cartCount = () => store.cart.reduce((n, l) => n + l.qty, 0);
const cartSubtotalCents = () => store.cart.reduce((c, l) => c + Math.round(Number(l.price) * 100) * l.qty, 0);
const imgUrl = (p) => (p.image && p.image.url) ? p.image.url : "";

// ── chrome (header bits + toast) ───────────────────────────────────────────────
function renderCartCount() {
  const n = cartCount(), pill = $("#cart-count");
  pill.textContent = n; pill.hidden = n === 0;
}
function renderAccount() {
  const a = $("#nav-account"), c = store.customer;
  if (c) { a.textContent = c.name.split(" ")[0] || "Account"; a.setAttribute("href", "#/profile"); }
  else { a.textContent = "Sign in"; a.setAttribute("href", "#/signup"); }
}
let toastTimer;
function toast(msg) {
  const t = $("#toast"); t.textContent = msg; t.hidden = false;
  clearTimeout(toastTimer); toastTimer = setTimeout(() => (t.hidden = true), 1800);
}

// ── views ──────────────────────────────────────────────────────────────────────
const app = () => $("#app");

// One live home state at a time. Each visit to the home view creates a fresh state;
// an in-flight loadMore from a previous visit checks `state === home` after its await
// and bails, so quick navigation can't double-render or blank the grid.
let home = null;
function viewHome() {
  app().innerHTML = `<div class="page-head"><div><h1>Our coffee &amp; tea</h1>
    <p class="muted">Fresh roasts and loose-leaf, shipped worldwide.</p></div></div>
    <div class="grid" id="grid"></div>
    <div class="load-more" id="more"></div>`;
  home = { cursor: "", count: 0, done: false, loading: false };
  loadMore();
}
async function loadMore() {
  const state = home;
  if (!state || state.loading || state.done) return;
  state.loading = true;
  const more = $("#more"); if (more) more.innerHTML = `<span class="muted">Loading…</span>`;
  try {
    const q = new URLSearchParams({ expand: "image", limit: PAGE });
    if (state.cursor) q.set("cursor", state.cursor);
    const { data, meta } = await api(`/products?${q}`);
    if (state !== home) return; // a newer home view superseded this fetch — drop it
    const grid = $("#grid"); if (!grid) return;
    data.forEach((p) => grid.appendChild(productCard(p)));
    state.count += data.length;
    state.cursor = (meta && meta.next_cursor) || "";
    state.done = !state.cursor;
    $("#more").innerHTML = state.done
      ? (state.count ? `<span class="muted">That's everything (${state.count} products).</span>` : `<span class="muted">No products yet.</span>`)
      : `<button class="btn btn-ghost" id="more-btn">Load more</button>`;
    const btn = $("#more-btn"); if (btn) btn.onclick = loadMore;
  } catch (e) {
    if (state === home && $("#more")) $("#more").innerHTML = `<span class="muted">Couldn't load products: ${esc(e.message)}</span>`;
  } finally {
    if (state === home) state.loading = false;
  }
}
function productCard(p) {
  const out = p.stock <= 0;
  const c = el(`<article class="card">
    <a href="#/p/${p.id}">${tile(p.name, imgUrl(p), "thumb")}</a>
    <div class="body">
      <a class="name" href="#/p/${p.id}">${esc(p.name)}</a>
      <div class="price">${money(p.price)}</div>
      <div class="row">
        ${out ? `<span class="badge-out">Sold out</span>` : `<button class="btn">Add to cart</button>`}
        <a class="link" href="#/p/${p.id}">Details</a>
      </div>
    </div></article>`);
  if (!out) c.querySelector("button").onclick = () => addToCart(p);
  return c;
}

async function viewProduct(id) {
  app().innerHTML = `<p class="muted">Loading…</p>`;
  try {
    const { data: p } = await api(`/products/${id}?expand=image`);
    const out = p.stock <= 0;
    app().innerHTML = "";
    const node = el(`<div>
      <a class="link" href="#/">&larr; Back to shop</a>
      <div class="detail" style="margin-top:14px">
        ${tile(p.name, imgUrl(p), "hero")}
        <div>
          <h1>${esc(p.name)}</h1>
          <div class="price">${money(p.price)}</div>
          <p class="muted">${esc(p.description || "A house favourite.")}</p>
          <p class="${out ? "badge-out" : "muted"}">${out ? "Sold out" : p.stock + " in stock"}</p>
          <div style="display:flex;gap:12px;align-items:center;margin-top:12px">
            <div class="qty"><button data-d="-1">−</button><span id="q">1</span><button data-d="1">+</button></div>
            <button class="btn" id="add" ${out ? "disabled" : ""}>Add to cart</button>
          </div>
        </div>
      </div></div>`);
    let q = 1;
    node.querySelectorAll(".qty button").forEach((b) => b.onclick = () => {
      q = Math.max(1, Math.min(p.stock || 99, q + Number(b.dataset.d))); $("#q", node).textContent = q;
    });
    node.querySelector("#add").onclick = () => { addToCart(p, q); location.hash = "#/cart"; };
    app().appendChild(node);
  } catch (e) { app().innerHTML = `<p class="center muted">Product not found.<br>${esc(e.message)}</p>`; }
}

function viewCart() {
  const cart = store.cart;
  if (!cart.length) { app().innerHTML = emptyState("Your cart is empty", "Browse the shop", "#/"); return; }
  app().innerHTML = `<div class="page-head"><h1>Your cart</h1></div><div class="panel" id="lines"></div>
    <div class="panel totals" style="margin-top:16px"><span>Subtotal</span><span id="sub"></span></div>
    <div style="margin-top:16px;display:flex;justify-content:flex-end">
      <button class="btn" id="co">Checkout</button></div>`;
  const lines = $("#lines");
  cart.forEach((l) => {
    const row = el(`<div class="line">
      ${tile(l.name, l.image, "cart-thumb")}
      <div class="grow"><div style="font-weight:700">${esc(l.name)}</div><div class="muted">${money(l.price)} each</div></div>
      <div class="qty"><button data-d="-1">−</button><span>${l.qty}</span><button data-d="1">+</button></div>
      <div style="width:80px;text-align:right;font-weight:700">${money(Number(l.price) * l.qty)}</div>
      <button class="link" title="Remove">✕</button></div>`);
    const btns = row.querySelectorAll(".qty button");
    btns[0].onclick = () => changeQty(l.id, -1); btns[1].onclick = () => changeQty(l.id, 1);
    row.querySelector(".link").onclick = () => changeQty(l.id, -l.qty);
    lines.appendChild(row);
  });
  $("#sub").textContent = money(cartSubtotalCents() / 100);
  $("#co").onclick = () => location.hash = "#/checkout";
}
function changeQty(id, d) {
  let cart = store.cart;
  const l = cart.find((x) => x.id === id); if (!l) return;
  l.qty += d; cart = cart.filter((x) => x.qty > 0);
  store.cart = cart; viewCart();
}

function viewSignup() {
  app().innerHTML = `<div class="narrow"><div class="page-head"><h1>Create your account</h1></div>
    <div class="panel"><div id="msg"></div>
      <div class="field"><label>Name</label><input id="name" placeholder="Ada Lovelace"></div>
      <div class="field"><label>Email</label><input id="email" type="email" placeholder="ada@example.com"></div>
      <button class="btn btn-block" id="go">Sign up</button>
      <p class="muted" style="margin-top:12px;font-size:13px">Demo only — no password. Creates a
      <code>customers</code> record via the public API.</p>
    </div></div>`;
  $("#go").onclick = async () => {
    const name = $("#name").value.trim(), email = $("#email").value.trim();
    if (!name || !email) return ($("#msg").innerHTML = `<div class="alert err">Name and email are required.</div>`);
    $("#go").disabled = true;
    try {
      const { data } = await api("/customers", { method: "POST", body: JSON.stringify({ name, email }) });
      store.customer = { id: data.id, name: data.name, email: data.email };
      toast("Welcome, " + name.split(" ")[0]);
      location.hash = store.cart.length ? "#/checkout" : "#/profile";
    } catch (e) {
      $("#msg").innerHTML = `<div class="alert err">${esc(e.message)}</div>`; $("#go").disabled = false;
    }
  };
}

function viewProfile() {
  const c = store.customer;
  if (!c) { location.hash = "#/signup"; return; }
  const orders = store.orders;
  app().innerHTML = `<div class="page-head"><h1>Hi, ${esc(c.name)}</h1><button class="btn btn-ghost" id="out">Sign out</button></div>
    <div class="panel"><div class="muted">Account</div>
      <div style="font-weight:700;margin-top:4px">${esc(c.name)}</div><div>${esc(c.email)}</div></div>
    <h2 style="margin-top:24px">Recent orders</h2>
    ${orders.length ? `<div class="panel" id="orders"></div>` : `<p class="muted">No orders yet.</p>`}`;
  $("#out").onclick = () => { store.customer = null; location.hash = "#/"; };
  if (orders.length) {
    const box = $("#orders");
    orders.slice().reverse().forEach((o) => box.appendChild(el(`<div class="line">
      <div class="grow"><div style="font-weight:700">Order ${esc((o.id || "").slice(0, 8))}</div>
        <div class="muted">${o.items} item(s) · ${esc(o.when)}</div></div>
      <div style="text-align:right"><div style="font-weight:800">${money(o.total)}</div>
        <div class="muted" style="font-size:12px">${esc(o.status)}</div></div></div>`)));
  }
}

function viewCheckout() {
  const cart = store.cart, c = store.customer;
  if (!cart.length) { app().innerHTML = emptyState("Nothing to check out", "Browse the shop", "#/"); return; }
  if (!c) { toast("Please sign in first"); location.hash = "#/signup"; return; }
  app().innerHTML = `<div class="narrow"><div class="page-head"><h1>Checkout</h1></div>
    <div class="panel"><div id="msg"></div>
      <div class="muted">Delivering to</div>
      <div style="font-weight:700;margin:4px 0 14px">${esc(c.name)} · ${esc(c.email)}</div>
      <div id="sumlines"></div>
      <div class="totals" style="border-top:1px solid var(--line);margin-top:8px"><span>Estimated total</span><span>${money(cartSubtotalCents() / 100)}</span></div>
      <div class="field" style="margin-top:16px"><label>Payment (demo)</label>
        <select id="pay"><option value="tok_live_ok">Card ending 4242 — succeeds</option>
        <option value="declined">Declined card — fails</option></select></div>
      <button class="btn btn-block" id="place">Place order</button>
      <p class="muted" style="margin-top:12px;font-size:13px">The server prices the order, checks stock and takes
      payment in the <code>checkout</code> hook — then returns the real total.</p>
    </div></div>`;
  const sum = $("#sumlines");
  cart.forEach((l) => sum.appendChild(el(`<div class="line"><div class="grow">${esc(l.name)} × ${l.qty}</div>
    <div style="font-weight:700">${money(Number(l.price) * l.qty)}</div></div>`)));
  $("#place").onclick = async () => {
    $("#place").disabled = true; $("#msg").innerHTML = "";
    const payload = {
      customer: c.id, pay_token: $("#pay").value,
      items: cart.map((l) => ({ product: l.id, qty: l.qty })),
    };
    try {
      const { data } = await api("/orders", { method: "POST", body: JSON.stringify(payload) });
      const orders = store.orders;
      orders.push({ id: data.id, total: data.total, status: data.status, items: cart.length, when: new Date().toLocaleString() });
      store.orders = orders;
      store.cart = [];
      app().innerHTML = `<div class="narrow center">
        <h1>Order confirmed 🎉</h1>
        <p class="muted">Order <code>${esc((data.id || "").slice(0, 8))}</code></p>
        <div class="panel" style="text-align:left;margin-top:16px">
          <div class="totals" style="padding:0"><span>Total charged</span><span>${money(data.total)}</span></div>
          <div class="muted" style="margin-top:6px">Status: <strong>${esc(data.status)}</strong> · priced &amp; stock-checked server-side</div>
        </div>
        <p style="margin-top:20px"><a class="btn" href="#/">Keep shopping</a></p></div>`;
    } catch (e) {
      const nice = e.status === 409 ? e.message
        : e.status === 402 ? "Payment was declined — try the other card."
        : e.message;
      $("#msg").innerHTML = `<div class="alert err">${esc(nice)}</div>`;
      $("#place").disabled = false;
    }
  };
}

function emptyState(title, cta, href) {
  return `<div class="center"><h1>${esc(title)}</h1><p style="margin-top:16px"><a class="btn" href="${href}">${esc(cta)}</a></p></div>`;
}

// ── router ───────────────────────────────────────────────────────────────────
function route() {
  const h = location.hash || "#/";
  window.scrollTo(0, 0);
  const m = h.match(/^#\/p\/(.+)$/);
  if (m) return viewProduct(m[1]);
  if (h.startsWith("#/cart")) return viewCart();
  if (h.startsWith("#/checkout")) return viewCheckout();
  if (h.startsWith("#/signup")) return viewSignup();
  if (h.startsWith("#/profile")) return viewProfile();
  return viewHome();
}
window.addEventListener("hashchange", route);
window.addEventListener("DOMContentLoaded", () => { renderCartCount(); renderAccount(); route(); });
