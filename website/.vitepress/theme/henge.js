// The home page's henge: one stone pair per cluster, raised, decorated with add-ons, linked and
// toppled. The stones are drawn in box-drawing characters, so the final frame is text a reader can
// select, and every frame of the animation is the same few strings rearranged.

const rep = (c, n) => c.repeat(n);
const W = 20; // columns of the stone laid on top
const POST = 6; // columns of each standing stone
const GAP = 6;
const EDGE = 1;
const N = 7; // rows of each standing stone below the one on top
const R = 4 + N + 1; // air, the top stone's three rows, the standing stones, the ground

const s = {
  lt: "┏" + rep("━", W - 2) + "┓",
  lm: "┃" + rep("░", W - 2) + "┃",
  lf: "┗" + rep("━", W - 2) + "┛",
  lj:
    "┗" +
    rep("━", EDGE - 1) +
    "┳" +
    rep("━", POST - 2) +
    "┳" +
    rep("━", GAP) +
    "┳" +
    rep("━", POST - 2) +
    "┳" +
    rep("━", EDGE - 1) +
    "┛",
  cap:
    rep(" ", EDGE) + "┏" + rep("━", POST - 2) + "┓" + rep(" ", GAP) + "┏" + rep("━", POST - 2) + "┓" + rep(" ", EDGE),
  up: rep(" ", EDGE) + "┃" + rep("░", POST - 2) + "┃" + rep(" ", GAP) + "┃" + rep("░", POST - 2) + "┃" + rep(" ", EDGE),
  g0: rep("━", W),
  g1: rep("━", EDGE) + "┻" + rep("━", POST - 2) + "┻" + rep("━", GAP) + "┻" + rep("━", POST - 2) + "┻" + rep("━", EDGE),
};
const B = rep(" ", W);

// The two standing stones h rows out of the ground, the topmost row their tops.
function rising(h) {
  const rows = Array(R - 1).fill(B);
  for (let i = 0; i < h; i++) rows[R - 2 - i] = i === h - 1 ? s.cap : s.up;
  return [...rows, h ? s.g1 : s.g0];
}
const RISE = N + 1;
const done = [B, s.lt, s.lm, s.lj, ...Array(N).fill(s.up), s.g1];
const falling = [s.lt, s.lm, s.lf];

const mid = Math.floor(W / 2);
const cracked = s.lm.slice(0, mid) + "╱" + s.lm.slice(mid + 1);
const ups = Array(N).fill(s.up);
const fallen = s.up.replace(/┃ +┃/u, (m) => "┃" + rep("▄", m.length - 2) + "┃");
const rubble = " " + "▄▄▄▄▄▄ ▄▄▄▄ ▄▄▄▄▄▄".padEnd(W - 2, " ") + " ";
const settle = " " + "▂ ▂▂▂  ▂▂ ▂▂▂ ▂ ▂▂".padEnd(W - 2, " ") + " ";
const toppling = [
  [B, s.lt, cracked, s.lj, ...ups, s.g1],
  [B, B, B, s.cap, ...ups.slice(1), fallen, s.g1],
  [B, B, B, B, ...Array(N - 1).fill(B), rubble, s.g1],
  [B, B, B, B, ...Array(N - 1).fill(B), settle, s.g0],
];
// Where the next cluster will stand: its outline, dashed.
const ghost = [...done.slice(0, -1).map((r) => r.replace(/━/gu, "╍").replace(/┃/gu, "╏").replace(/░/gu, " ")), s.g0];

function paint(rows, ground = true) {
  const last = rows.length - 1;
  return rows
    .map((r, i) => {
      if (ground && i === last) return ("━" + r + "━").replace(/━+/gu, (m) => `<b class="g">${m}</b>`);
      const line = " " + r + " ";
      // A row of air still takes its line, also where an editor trims trailing spaces.
      if (line.trim() === "") return "&nbsp;";
      return line
        .replace(/░+/gu, (m) => `<i>${m}</i>`)
        .replace(/[▄▂]+/gu, (m) => `<s>${m}</s>`)
        .replace(/╱/gu, "<em>╱</em>");
    })
    .join("\n");
}

// Columns and rows of the painted drawing (one column of margin each side), for the dust.
const at = { ground: R - 1, joint: 3, feet: [2, 7, 14, 19], ends: [1, W] };

const DRESS = ["Headlamp", "Monitoring", "Istio", "Pod security"];
const FLAGS_PER_STONE = 2;
const MAX = 5;
const SPIN = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏";
const PHASE = { Pending: "pend", Provisioning: "pend", Provisioned: "ok", Deleting: "del" };
const BITS = ["░", "▒", "▘", "▝", "▖", "▗", "·", "˙", "▂"];
const SPARKS = ["✦", "✧", "·", "˙", "+"];
const NS = "http://www.w3.org/2000/svg";

// Starts the page's motion inside root and returns what stops it, for when the page is left.
export function start(root) {
  const still = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  const timers = new Set();
  const intervals = new Set();
  const observers = [];
  let stopped = false;
  // A wait on a page that has been left never resolves, so nothing after it runs.
  const wait = (ms) =>
    new Promise((resolve) => {
      if (stopped) return;
      const t = setTimeout(
        () => {
          timers.delete(t);
          resolve();
        },
        still ? 0 : ms,
      );
      timers.add(t);
    });
  const $ = (q, el = root) => el.querySelector(q);
  const $$ = (q, el = root) => [...el.querySelectorAll(q)];
  const anim = (el, frames, opts) => (still || !el.animate ? null : el.animate(frames, opts));

  $$("[data-copy]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const src = $(btn.dataset.copy, btn.closest("[data-copy-scope]") ?? root);
      const text = src?.dataset.text ?? src?.textContent ?? "";
      try {
        await navigator.clipboard.writeText(text.trim());
        btn.dataset.state = "copied";
      } catch {
        btn.dataset.state = "failed";
      }
      timers.add(setTimeout(() => delete btn.dataset.state, 1600));
    });
  });

  // The install line types itself while the first stones go up, keeping its break points, so it
  // wraps at the same places throughout.
  (async () => {
    const cmd = $(".cmdbox .cmd");
    if (!cmd || still) return;
    const segs = [...cmd.childNodes].filter((n) => n.nodeType === Node.TEXT_NODE).map((n) => n.textContent);
    const full = segs.join("");
    const esc = (t) => t.replace(/&/g, "&amp;").replace(/</g, "&lt;");
    const upTo = (i) => {
      let out = "";
      let n = 0;
      for (const sg of segs) {
        if (n >= i) break;
        out += (n ? "<wbr>" : "") + esc(sg.slice(0, i - n));
        n += sg.length;
      }
      return out;
    };
    cmd.textContent = "";
    await wait(500);
    for (let i = 1; i <= full.length; i++) {
      cmd.innerHTML = upTo(i);
      await wait(i < 10 ? 45 : 11);
    }
  })();

  // The interface rises when it comes into view.
  $$("[data-rise]").forEach((el) => {
    if (still) return;
    el.classList.add("sunk");
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          io.disconnect();
          el.classList.remove("sunk");
        }
      },
      { threshold: 0.2 },
    );
    io.observe(el);
    observers.push(io);
  });

  const list = $("[data-ring]");
  const henge = list.closest(".henge");
  const ley = $("[data-ley]");
  const said = $("[data-said]");
  const raiseBtn = $("[data-act=raise]");
  const decoBtn = $("[data-act=decorate]");
  const meshBtn = $("[data-act=connect]");
  const toppleBtn = $("[data-act=topple]");

  const initial = [
    { name: "prod", dist: "RKE2", ver: "v1.37.1+rke2r1", decos: ["Rancher"] },
    { name: "staging", dist: "k3s", ver: "v1.37.1+k3s1", decos: ["Headlamp"] },
    { name: "ci", dist: "kubeadm", ver: "v1.36.5", decos: ["Monitoring"] },
    { name: "dev", dist: "kubeadm", ver: "v1.37.1", decos: [] },
  ];
  const queue = [{ name: "web", dist: "k3s", ver: "v1.37.1+k3s1", decos: [] }];
  const order = [];
  const links = [];
  let busy = true;
  let loading = true;

  const slots = Array.from({ length: MAX }, () => {
    const li = document.createElement("li");
    li.innerHTML =
      '<div class="aw"><pre class="art" aria-hidden="true"></pre><pre class="top-stone" aria-hidden="true" hidden></pre><div class="decos" aria-hidden="true"></div></div>' +
      '<div class="lab"><p class="nm"></p><p class="ds"></p><p class="ph"></p></div>';
    list.append(li);
    return {
      li,
      aw: $(".aw", li),
      art: $(".art", li),
      top: $(".top-stone", li),
      decos: $(".decos", li),
      nm: $(".nm", li),
      ds: $(".ds", li),
      ph: $(".ph", li),
      c: null,
      spin: 0,
    };
  });

  const draw = (sl, rows) => (sl.art.innerHTML = paint(rows));

  function flag(sl, name) {
    const f = document.createElement("span");
    f.className = `flag ${name.toLowerCase().replace(/ /g, "-")}`;
    f.innerHTML = `<span>${name}</span>`;
    sl.decos.append(f);
    return f;
  }

  function label(sl) {
    const c = sl.c;
    clearInterval(sl.spin);
    intervals.delete(sl.spin);
    if (!c) return;
    sl.nm.textContent = c.name;
    sl.ds.textContent = `${c.dist} ${c.ver}`;
    sl.ph.className = `ph ${PHASE[c.phase]}`;
    sl.ph.textContent = c.phase;
    const addons = c.decos.length ? `, with ${c.decos.join(" and ")}` : "";
    sl.li.setAttribute("aria-label", `${c.name}: ${c.dist} ${c.ver}${addons}, ${c.phase}`);
    if (c.phase === "Provisioning" && !still) {
      let k = 0;
      sl.spin = setInterval(() => (sl.ph.textContent = `Provisioning ${SPIN[k++ % SPIN.length]}`), 80);
      intervals.add(sl.spin);
    }
  }

  function empty(sl, asGhost) {
    clearInterval(sl.spin);
    intervals.delete(sl.spin);
    sl.c = null;
    sl.decos.replaceChildren();
    sl.li.className = asGhost ? "slot ghost" : "slot bare";
    sl.li.setAttribute("aria-label", "empty: room for another cluster");
    draw(sl, asGhost ? ghost : rising(0));
    sl.nm.textContent = asGhost ? "next" : "";
    sl.ds.textContent = asGhost ? "room for one more" : "";
    sl.ph.textContent = "";
    sl.ph.className = "ph";
  }

  // Dust in block characters, or sparks, thrown from a column and row of the drawing.
  function dust(sl, col, row, n, spread, lift, bits = BITS) {
    if (still) return;
    for (let k = 0; k < n; k++) {
      const d = document.createElement("span");
      d.className = bits === SPARKS ? "dust spark" : "dust";
      d.textContent = bits[Math.floor(Math.random() * bits.length)];
      d.style.left = `${col * 0.6}em`;
      d.style.top = `${row * 1.32}em`;
      sl.aw.append(d);
      const dx = (Math.random() - 0.5) * 2 * spread;
      const dy = -(0.3 + Math.random()) * lift;
      const a = d.animate(
        [
          { transform: "translate(0, 0)", opacity: 1 },
          { transform: `translate(${dx}em, ${dy}em)`, opacity: 0.85, offset: 0.55 },
          { transform: `translate(${dx * 1.25}em, ${dy * 0.6 + 0.4}em)`, opacity: 0 },
        ],
        { duration: 650 + Math.random() * 600, easing: "cubic-bezier(.15,.7,.3,1)" },
      );
      a.onfinish = () => d.remove();
    }
  }

  const quake = (power) =>
    anim(
      henge,
      [
        { transform: "translate(0, 0)" },
        { transform: `translate(${-power}px, ${power}px)` },
        { transform: `translate(${power}px, ${-power / 2}px)` },
        { transform: `translate(${-power / 2}px, ${power / 2}px)` },
        { transform: `translate(${power / 3}px, 0)` },
        { transform: "translate(0, 0)" },
      ],
      { duration: 380, easing: "ease-out" },
    );

  // Links between clusters run underground, as arcs from foot to foot: Rancher's registrations
  // solid, Cluster Mesh dashed, and the further apart two stones stand, the deeper the arc.
  function foot(sl) {
    const h = henge.getBoundingClientRect();
    const r = sl.aw.getBoundingClientRect();
    return { x: r.left + r.width / 2 - h.left, y: r.bottom - h.top };
  }
  function shape(l) {
    const a = foot(l.a);
    const b = foot(l.b);
    l.span = Math.abs(b.x - a.x) / henge.getBoundingClientRect().width;
    const depth = (l.kind === "mesh" ? 10 : 14) + l.span * 46;
    l.path.setAttribute("d", `M${a.x} ${a.y} Q${(a.x + b.x) / 2} ${a.y + depth * 2} ${b.x} ${b.y}`);
    // A quadratic arc bottoms out at half its control point's depth; the label hangs just below.
    l.text.setAttribute("x", `${(a.x + b.x) / 2}`);
    l.text.setAttribute("y", `${a.y + depth + 13}`);
  }
  // Every mesh line carries its name; Rancher's lines all say the same, so only the widest does.
  function relabel() {
    const widest = links.filter((l) => l.kind === "rancher").reduce((w, l) => (!w || l.span > w.span ? l : w), null);
    for (const l of links) l.text.style.display = l.kind === "mesh" || l === widest ? "" : "none";
  }
  function relayout() {
    links.forEach(shape);
    relabel();
  }
  function link(a, b, kind) {
    const g = document.createElementNS(NS, "g");
    g.setAttribute("class", `link ${kind}`);
    const path = document.createElementNS(NS, "path");
    const text = document.createElementNS(NS, "text");
    text.textContent = kind === "mesh" ? "Cilium Cluster Mesh" : "registered with Rancher";
    g.append(path, text);
    ley.append(g);
    const l = { a, b, kind, g, path, text, span: 0 };
    links.push(l);
    shape(l);
    relabel();
    if (kind === "rancher") {
      const len = path.getTotalLength();
      anim(
        path,
        [
          { strokeDasharray: `${len}`, strokeDashoffset: `${len}` },
          { strokeDasharray: `${len}`, strokeDashoffset: "0" },
        ],
        { duration: 700, easing: "ease-out" },
      );
    } else {
      anim(g, [{ opacity: 0 }, { opacity: 1 }], { duration: 500 });
    }
  }
  function unlink(sl) {
    for (const l of links.filter((x) => x.a === sl || x.b === sl)) {
      links.splice(links.indexOf(l), 1);
      const out = anim(l.g, [{ opacity: 1 }, { opacity: 0 }], { duration: 400, fill: "forwards" });
      if (out) out.onfinish = () => l.g.remove();
      else l.g.remove();
    }
    relabel();
  }
  const ro = new ResizeObserver(relayout);
  ro.observe(henge);
  observers.push(ro);

  const hub = () => order.find((sl) => sl.c?.decos.includes("Rancher") && sl.c.phase === "Provisioned");
  function register(sl) {
    const r = hub();
    if (!r || r === sl || sl.c.decos.includes("Rancher")) return;
    link(r, sl, "rancher");
    said.textContent = `${sl.c.name} registered with Rancher`;
  }
  const meshed = (a, b) =>
    links.some((l) => l.kind === "mesh" && ((l.a === a && l.b === b) || (l.a === b && l.b === a)));
  function pair() {
    const up = order.filter((sl) => sl.c?.phase === "Provisioned").sort((x, y) => slots.indexOf(x) - slots.indexOf(y));
    for (let i = 0; i + 1 < up.length; i++) if (!meshed(up[i], up[i + 1])) return [up[i], up[i + 1]];
    return null;
  }

  // A flag drops onto the stone on top, bites in and swings itself still.
  async function hang(sl, name) {
    const f = flag(sl, name);
    const col = (f.offsetLeft / sl.aw.offsetWidth) * 22 + 1;
    const drop = anim(
      f,
      [
        { transform: "translateY(-9em) rotate(-25deg)", opacity: 0 },
        { transform: "translateY(-6em) rotate(-18deg)", opacity: 1, offset: 0.3 },
        { transform: "translateY(0) rotate(0deg)", opacity: 1 },
      ],
      { duration: 420, easing: "cubic-bezier(.55,0,1,.5)" },
    );
    if (drop) await drop.finished;
    quake(2);
    dust(sl, col, 1, 4, 1.5, 0.8);
    dust(sl, col + 2, 0, 9, 3.5, 2.2, SPARKS);
    anim(
      f,
      [
        { transform: "rotate(0deg)" },
        { transform: "rotate(14deg)" },
        { transform: "rotate(-9deg)" },
        { transform: "rotate(5deg)" },
        { transform: "rotate(-2deg)" },
        { transform: "rotate(0deg)" },
      ],
      { duration: 900, easing: "ease-out" },
    );
    await wait(300);
  }

  async function raise(sl, spec) {
    const c = { ...spec, decos: [...spec.decos], phase: "Pending" };
    sl.c = c;
    order.push(sl);
    sl.li.className = "slot rising bare";
    draw(sl, rising(0));
    label(sl);
    await wait(30);
    sl.li.classList.remove("bare");
    dust(sl, 10, at.ground, 6, 5, 1.2);
    await wait(380);
    c.phase = "Provisioning";
    label(sl);
    // Out of the ground, a row at a time, each row a shove.
    for (let h = 1; h <= RISE; h++) {
      draw(sl, rising(h));
      for (const f of at.feet) dust(sl, f, at.ground, 1, 1.6, 1.4);
      anim(sl.aw, [{ transform: "translateY(0.06em)" }, { transform: "translateY(0)" }], { duration: 90 });
      await wait(h === RISE ? 220 : 95);
    }
    // The stone on top comes down from well above and lands with its weight.
    sl.top.innerHTML = paint(falling, false);
    sl.top.hidden = false;
    const fall = anim(
      sl.top,
      [
        { transform: "translateY(-15em) rotate(-4deg)", opacity: 0 },
        { transform: "translateY(-11em) rotate(-3deg)", opacity: 1, offset: 0.25 },
        { transform: "translateY(0) rotate(0deg)", opacity: 1 },
      ],
      { duration: 520, easing: "cubic-bezier(.55,0,1,.5)" },
    );
    if (fall) await fall.finished;
    sl.top.hidden = true;
    draw(sl, done);
    anim(
      sl.aw,
      [
        { transform: "scale(1.04, 0.93)" },
        { transform: "scale(0.99, 1.02)", offset: 0.45 },
        { transform: "scale(1, 1)" },
      ],
      {
        duration: 340,
        easing: "ease-out",
      },
    );
    quake(5);
    for (const e of at.ends) dust(sl, e, at.joint, 7, 3.2, 2.4);
    for (const f of at.feet) dust(sl, f, at.ground, 3, 2.6, 1.6);
    await wait(260);
    c.phase = "Provisioned";
    sl.li.className = "slot up landed";
    label(sl);
    for (const name of c.decos) {
      await wait(200);
      await hang(sl, name);
    }
    register(sl);
    if (c.decos.includes("Rancher")) order.filter((x) => x !== sl && x.c?.phase === "Provisioned").forEach(register);
  }

  const nextDress = (sl) => DRESS.find((d) => !sl.c.decos.includes(d));

  async function decorate(sl) {
    const name = nextDress(sl);
    sl.c.decos.push(name);
    label(sl);
    await hang(sl, name);
  }

  async function topple(sl) {
    const c = sl.c;
    c.phase = "Deleting";
    sl.li.className = "slot del";
    label(sl);
    unlink(sl);
    const [crack, drop, heap, dusted] = toppling;
    draw(sl, crack);
    for (const f of sl.decos.children) {
      anim(f, [{ transform: "rotate(0)" }, { transform: "translate(1.5em, 3em) rotate(80deg)", opacity: 0 }], {
        duration: 600,
        easing: "cubic-bezier(.5,0,1,.6)",
        fill: "forwards",
      });
    }
    anim(
      sl.aw,
      [
        { transform: "rotate(0)" },
        { transform: "rotate(-1deg)" },
        { transform: "rotate(1deg)" },
        { transform: "rotate(0)" },
      ],
      {
        duration: 420,
        iterations: 2,
      },
    );
    await wait(700);
    draw(sl, drop);
    quake(4);
    dust(sl, 10, at.ground - 1, 10, 5, 2);
    await wait(360);
    draw(sl, heap);
    quake(6);
    for (const f of at.feet) dust(sl, f, at.ground, 4, 3, 2.2);
    await wait(420);
    draw(sl, dusted);
    await wait(520);
    order.splice(order.indexOf(sl), 1);
    queue.unshift({ name: c.name, dist: c.dist, ver: c.ver, decos: c.decos });
    empty(sl, true);
    anim(sl.aw, [{ opacity: 0 }, { opacity: 1 }], { duration: 500 });
  }

  const victim = () => [...order].reverse().find((sl) => sl.c && sl.c.name !== "prod");
  const plain = () =>
    [...order]
      .reverse()
      .find((sl) => sl.c?.phase === "Provisioned" && sl.c.decos.length < FLAGS_PER_STONE && nextDress(sl));

  function controls() {
    const free = slots.find((sl) => !sl.c);
    const next = queue[0];
    $("code", raiseBtn).textContent =
      free && next ? `kubectl apply -f ${next.name}.yaml` : "the ring is full: topple one first";
    raiseBtn.disabled = busy || !free || !next;
    const bare = plain();
    if (bare) {
      const d = nextDress(bare);
      $("code", decoBtn).textContent = `${d.toLowerCase()}: on, in ${bare.c.name}`;
    } else if (!loading) {
      $("code", decoBtn).textContent = "every stone is dressed";
    }
    decoBtn.disabled = busy || !bare;
    const p = pair();
    if (p) $("code", meshBtn).textContent = `cluster mesh: ${p[0].c.name} ↔ ${p[1].c.name}`;
    else if (!loading) $("code", meshBtn).textContent = "every neighbour is joined";
    meshBtn.disabled = busy || !p;
    const v = victim();
    if (!loading)
      $("code", toppleBtn).textContent = v
        ? `kubectl -n clusters delete cluster ${v.c.name}`
        : "nothing left to topple but prod";
    toppleBtn.disabled = busy || !v;
  }

  async function run(f) {
    if (busy) return;
    busy = true;
    controls();
    try {
      await f();
    } finally {
      busy = false;
      controls();
    }
  }

  raiseBtn.addEventListener("click", () =>
    run(async () => {
      const spec = queue.shift();
      said.textContent = `cluster.cluster.x-k8s.io/${spec.name} created`;
      await raise(
        slots.find((x) => !x.c),
        spec,
      );
    }),
  );
  decoBtn.addEventListener("click", () =>
    run(async () => {
      const sl = plain();
      said.textContent = `cluster.cluster.x-k8s.io/${sl.c.name} patched`;
      await decorate(sl);
    }),
  );
  meshBtn.addEventListener("click", () =>
    run(async () => {
      const [a, b] = pair();
      link(a, b, "mesh");
      quake(2);
      said.textContent = `${a.c.name} and ${b.c.name} share one pod network`;
      await wait(500);
    }),
  );
  toppleBtn.addEventListener("click", () =>
    run(async () => {
      const sl = victim();
      said.textContent = `cluster.cluster.x-k8s.io "${sl.c.name}" deleted`;
      await topple(sl);
    }),
  );

  slots.forEach((sl) => empty(sl, false));

  if (still) {
    initial.forEach((spec, i) => {
      slots[i].c = { ...spec, decos: [...spec.decos], phase: "Provisioned" };
      order.push(slots[i]);
      slots[i].li.className = "slot up";
      draw(slots[i], done);
      spec.decos.forEach((name) => flag(slots[i], name));
      label(slots[i]);
    });
    order.forEach(register);
    link(slots[2], slots[3], "mesh");
    said.textContent = "";
    empty(slots[MAX - 1], true);
    busy = loading = false;
    controls();
  } else {
    controls();
    (async () => {
      await wait(450);
      const going = [];
      for (let i = 0; i < initial.length; i++) {
        going.push(raise(slots[i], initial[i]));
        await wait(1050);
      }
      await Promise.all(going);
      await wait(300);
      link(slots[2], slots[3], "mesh");
      quake(2);
      said.textContent = "ci and dev share one pod network";
      await wait(400);
      empty(slots[MAX - 1], true);
      anim(slots[MAX - 1].aw, [{ opacity: 0 }, { opacity: 1 }], { duration: 700 });
      busy = loading = false;
      controls();
    })();
  }

  return () => {
    stopped = true;
    timers.forEach(clearTimeout);
    intervals.forEach(clearInterval);
    observers.forEach((o) => o.disconnect());
  };
}
