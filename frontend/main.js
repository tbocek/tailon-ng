"use strict";

// Tailon-ng frontend: framework-free vanilla JavaScript. It fetches the file list
// and streams lines over Server-Sent Events. Modes: "tail" (follow) and "grep"
// (whole file); a regexp filter (server-side, invertible) narrows the output.
// Injected global: relativeRoot.

const RELATIVE_ROOT = (typeof relativeRoot !== "undefined" && relativeRoot) || "/";
// "tail" follows live files; "grep" reads them whole; "grep-all" additionally
// reads rotated/compressed archives (.gz, .1, …), decoded server-side.
const MODES = ["tail", "grep", "grep-all"];
const TAIL_LINES = 10; // trailing lines shown when a tail starts (grep ignores it)
const MAX_LINES = 50000; // browser-side scrollback cap

const state = {
    files: [], file: null, mode: "tail", filter: "", invert: false, wrap: false,
    source: null,
    prefix: "", // directory prefix shared by every served file, hidden in the UI
};

// Served files are append-only, so lines fetched once stay valid: single-file
// views are cached per (file, mode, filter, invert) along with the byte offset
// read so far, and reconnecting re-renders the cache and asks the server only
// for what follows. Archives are immutable — once read, they never re-fetch.
// The server sends "event: reset" if a file shrank or was replaced.
const MAX_CACHE_VIEWS = 20;
const cache = new Map(); // key -> { lines: [], offset: -1, done: false }

function cacheEntry() {
    const m = state.mode === "tail" ? "tail" : "grep"; // grep-all == grep for one file
    const key = JSON.stringify([state.file.path, m, state.filter, state.invert]);
    let entry = cache.get(key);
    if (entry) cache.delete(key); // re-insert, so eviction drops the least recent
    else entry = { lines: [], offset: -1, done: false };
    cache.set(key, entry);
    while (cache.size > MAX_CACHE_VIEWS) cache.delete(cache.keys().next().value);
    return entry;
}

const el = {};
[
    "file-select", "mode-select", "filter-input", "filter-apply",
    "cfg-invert", "cfg-wrap", "action-download", "status", "scrollable", "logview",
].forEach(function (id) { el[id] = document.getElementById(id); });

// ANSI color support. Tools like Caddy emit SGR escape sequences such as
// "\x1b[34mINFO\x1b[0m" (blue "INFO", then reset). We translate the color/style
// codes into styled spans so they render as colors instead of raw escape bytes.
// The standard 16 colors map to CSS classes (.ansi-fg-N / .ansi-bg-N) so the
// palette lives in the stylesheet; 256-color and truecolor fall back to inline
// styles. We build real DOM nodes (never innerHTML), so log text can't inject.
const ANSI_RE = /\x1b\[([0-9;:]*)([A-Za-z])/g;

function ansiReset() { return { bold: false, dim: false, italic: false, underline: false, fg: null, bg: null }; }
function ansiStyled(s) { return s.bold || s.dim || s.italic || s.underline || s.fg !== null || s.bg !== null; }

// xterm 256-color index (16..255) → "rgb(...)"; 0..15 use the class palette.
function ansi256(n) {
    if (n >= 232) { const v = 8 + (n - 232) * 10; return "rgb(" + v + "," + v + "," + v + ")"; }
    n -= 16;
    const f = function (c) { return c ? 55 + c * 40 : 0; };
    return "rgb(" + f(Math.floor(n / 36) % 6) + "," + f(Math.floor(n / 6) % 6) + "," + f(n % 6) + ")";
}

// Apply one SGR sequence's parameters (e.g. "1;34") to the running style state.
function ansiApply(style, params) {
    const codes = params.split(/[;:]/);
    for (let i = 0; i < codes.length; i++) {
        const n = parseInt(codes[i] || "0", 10);
        if (n === 0) Object.assign(style, ansiReset());
        else if (n === 1) style.bold = true;
        else if (n === 2) style.dim = true;
        else if (n === 3) style.italic = true;
        else if (n === 4) style.underline = true;
        else if (n === 22) { style.bold = false; style.dim = false; }
        else if (n === 23) style.italic = false;
        else if (n === 24) style.underline = false;
        else if (n >= 30 && n <= 37) style.fg = n - 30;
        else if (n >= 90 && n <= 97) style.fg = n - 90 + 8;
        else if (n === 39) style.fg = null;
        else if (n >= 40 && n <= 47) style.bg = n - 40;
        else if (n >= 100 && n <= 107) style.bg = n - 100 + 8;
        else if (n === 49) style.bg = null;
        else if (n === 38 || n === 48) { // extended: 38;5;N (256) or 38;2;R;G;B (truecolor)
            const key = n === 38 ? "fg" : "bg";
            if (codes[i + 1] === "5") { const idx = parseInt(codes[i + 2], 10); style[key] = idx < 16 ? idx : ansi256(idx); i += 2; }
            else if (codes[i + 1] === "2") { style[key] = "rgb(" + (+codes[i + 2] || 0) + "," + (+codes[i + 3] || 0) + "," + (+codes[i + 4] || 0) + ")"; i += 4; }
        }
    }
}

// Append `text` to `parent` as a span carrying the current style (or a bare text
// node when nothing is active). Numeric colors become classes; strings inline.
function ansiEmit(parent, text, style) {
    if (!text) return;
    if (!ansiStyled(style)) { parent.appendChild(document.createTextNode(text)); return; }
    const span = document.createElement("span");
    const cls = [];
    if (style.bold) cls.push("ansi-bold");
    if (style.dim) cls.push("ansi-dim");
    if (style.italic) cls.push("ansi-italic");
    if (style.underline) cls.push("ansi-underline");
    if (typeof style.fg === "number") cls.push("ansi-fg-" + style.fg);
    else if (style.fg) span.style.color = style.fg;
    if (typeof style.bg === "number") cls.push("ansi-bg-" + style.bg);
    else if (style.bg) span.style.backgroundColor = style.bg;
    if (cls.length) span.className = cls.join(" ");
    span.textContent = text;
    parent.appendChild(span);
}

// Parse SGR escape codes in `text` and append the styled result to `parent`.
function appendAnsi(parent, text) {
    if (text.indexOf("\x1b") === -1) { parent.appendChild(document.createTextNode(text)); return; }
    const style = ansiReset();
    let last = 0, m;
    ANSI_RE.lastIndex = 0;
    while ((m = ANSI_RE.exec(text)) !== null) {
        if (m.index > last) ansiEmit(parent, text.slice(last, m.index), style);
        if (m[2] === "m") ansiApply(style, m[1]); // ignore non-SGR sequences (cursor moves, etc.)
        last = ANSI_RE.lastIndex;
    }
    if (last < text.length) ansiEmit(parent, text.slice(last), style);
}

// Log view: append-only lines, auto-scrolling while you're at the bottom.
// Rendering is batched per animation frame: incoming lines queue in `pending`
// and flush as one DocumentFragment with a single scroll check. The naive way —
// append and scroll-check per line — forces a reflow for every line, and grep
// delivers tens of thousands of lines per second; likewise, trimming the
// scrollback with shift()/removeChild per line costs O(buffer) per line once
// the cap is reached. Batching plus chunked splicing keeps both amortized.
const TRIM_CHUNK = 1000; // trim the scrollback this many lines at a time
const logview = {
    lines: [], // rendered spans, oldest first
    pending: [], // {path, text} queued for the next animation frame
    raf: 0,
    atBottom: function () {
        const p = el["scrollable"];
        return Math.abs(p.scrollTop - (p.scrollHeight - p.offsetHeight)) < 50;
    },
    clear: function () {
        if (this.raf) { cancelAnimationFrame(this.raf); this.raf = 0; }
        this.pending = [];
        el["logview"].replaceChildren();
        this.lines = [];
    },
    // write queues one line; path (set in multi-file streams) becomes a
    // clickable prefix that jumps to grepping just that file (one delegated
    // click listener in init handles all of them — no per-line closure).
    write: function (path, text) {
        this.pending.push({ path: path, text: text });
        // A hidden tab gets no animation frames; keep the queue bounded.
        if (this.pending.length > MAX_LINES + TRIM_CHUNK) {
            this.pending.splice(0, this.pending.length - MAX_LINES);
        }
        if (!this.raf) this.raf = requestAnimationFrame(this.flush.bind(this));
    },
    flush: function () {
        this.raf = 0;
        const scroll = this.atBottom();
        const frag = document.createDocumentFragment();
        for (const ln of this.pending) {
            const span = document.createElement("span");
            span.className = "log-entry";
            if (ln.path) {
                const link = document.createElement("a");
                link.className = "path-link";
                link.textContent = stripPrefix(ln.path);
                link.title = "grep " + ln.path;
                link.dataset.path = ln.path;
                span.appendChild(link);
                span.appendChild(document.createTextNode(": "));
            }
            appendAnsi(span, ln.text);
            frag.appendChild(span);
            this.lines.push(span);
        }
        this.pending = [];
        el["logview"].appendChild(frag);
        if (this.lines.length > MAX_LINES + TRIM_CHUNK) {
            for (const old of this.lines.splice(0, this.lines.length - MAX_LINES)) old.remove();
        }
        if (scroll) el["scrollable"].scrollTop = el["scrollable"].scrollHeight;
    },
};

function setStatus(text) { el["status"].textContent = text; el["status"].hidden = !text; }

function connect() {
    if (state.source) { state.source.close(); state.source = null; }
    if (!state.file) return;
    logview.clear();

    const p = new URLSearchParams({ mode: state.mode, filter: state.filter, nlines: String(TAIL_LINES) });
    if (state.invert) p.set("invert", "1");

    let entry = null; // aggregate views are not cached: per-file offsets don't compose
    if (state.file.all) {
        p.set("all", "1");
        if (state.file.scope) p.set("scope", state.file.scope); // one subfolder only
    } else {
        p.set("path", state.file.path);
        entry = cacheEntry();
        for (const t of entry.lines) logview.write(null, t); // replay the cache (one batched flush)
        if (entry.done) return; // a fully-read archive never grows: no request at all
        if (entry.offset >= 0) p.set("offset", String(entry.offset));
    }

    setStatus("connecting…");
    const src = new EventSource(RELATIVE_ROOT + "stream?" + p.toString());
    state.source = src;
    src.onopen = function () { setStatus(""); };
    src.onmessage = function (e) {
        const d = JSON.parse(e.data);
        if (entry) {
            entry.lines.push(d.t);
            if (entry.lines.length > MAX_LINES + TRIM_CHUNK) {
                entry.lines.splice(0, entry.lines.length - MAX_LINES); // chunked, not per line
            }
            if (d.o) entry.offset = d.o;
        }
        logview.write(d.p, d.t);
    };
    src.addEventListener("reset", function () {
        // The file shrank or was replaced: everything cached is invalid.
        if (entry) { entry.lines = []; entry.offset = -1; }
        logview.clear();
    });
    src.addEventListener("eof", function () {
        if (entry && state.file && state.file.stale) entry.done = true; // archives are immutable
        src.close(); state.source = null; setStatus("");
    });
    src.onerror = function () { setStatus("reconnecting…"); };
}

// stripPrefix hides the directory prefix common to every served file — with a
// single tree like /var/log/remote/ the noise-free remainder is what you read.
function stripPrefix(path) {
    return state.prefix && path.indexOf(state.prefix) === 0 ? path.slice(state.prefix.length) : path;
}

// commonPrefix returns the directory prefix (up to a "/") shared by all paths,
// or "" when there is none (or just one path component).
function commonPrefix(paths) {
    if (!paths.length) return "";
    let p = paths[0];
    paths.forEach(function (q) { while (p && q.indexOf(p) !== 0) p = p.slice(0, -1); });
    return p.slice(0, p.lastIndexOf("/") + 1);
}

// jumpToFile selects the file in the dropdown and greps it (used by the
// clickable per-line path prefix in multi-file streams).
function jumpToFile(path) {
    const i = state.files.findIndex(function (f) { return !f.all && f.path === path; });
    if (i < 0) return;
    el["file-select"].value = String(i);
    state.file = state.files[i];
    state.mode = "grep";
    el["mode-select"].value = "grep";
    updateDownload();
    syncModeOptions();
    connect();
}

// syncModeOptions disables "tail" while a rotated/compressed file is selected
// (it will never grow, so it can only be grepped) and switches to grep. It only
// adjusts state — the caller connects.
function syncModeOptions() {
    const stale = state.file && state.file.stale;
    el["mode-select"].options[MODES.indexOf("tail")].disabled = !!stale;
    if (stale && state.mode === "tail") {
        state.mode = "grep";
        el["mode-select"].value = "grep";
    }
}

async function refreshFiles() {
    let data;
    try { data = await (await fetch(RELATIVE_ROOT + "list")).json(); }
    catch (e) { setStatus("could not load file list"); return; }

    const prev = state.file && (state.file.scope || state.file.path);
    state.files = [];
    state.prefix = commonPrefix(data.map(function (e) { return e.path; }));
    el["file-select"].replaceChildren();

    el["file-select"].add(new Option("All files", "0"));
    state.files.push({ path: "", all: true });

    // Offer each subfolder as a "tail/grep everything under here" entry. A folder
    // is any ancestor directory holding some — but not all — of the files; one
    // holding all of them would just duplicate "All files", so it is skipped.
    const counts = {};
    data.forEach(function (entry) {
        let d = entry.path;
        for (let i = d.lastIndexOf("/"); i > 0; i = d.lastIndexOf("/")) {
            d = d.slice(0, i);
            counts[d] = (counts[d] || 0) + 1;
        }
    });
    Object.keys(counts).filter(function (d) { return counts[d] < data.length; }).sort()
        .forEach(function (d) {
            el["file-select"].add(new Option("▸ " + stripPrefix(d) + "/", String(state.files.length)));
            state.files.push({ path: d, scope: d, all: true });
        });

    data.forEach(function (entry) {
        const label = stripPrefix(entry.path) + (entry.stale ? "  (archived)" : "");
        el["file-select"].add(new Option(label, String(state.files.length)));
        state.files.push(entry);
    });

    // Restore the previous selection by path/scope, else select the first entry.
    let i = state.files.findIndex(function (f) { return (f.scope || f.path) === prev; });
    if (i < 0) i = state.files.length ? 0 : -1;
    state.file = i >= 0 ? state.files[i] : null;
    if (i >= 0) el["file-select"].value = String(i);
    syncModeOptions();
}

function updateDownload() {
    const off = !state.file || state.file.all;
    el["action-download"].hidden = off;
    if (!off) {
        el["action-download"].href = RELATIVE_ROOT + "files/?path=" + encodeURIComponent(state.file.path);
        el["action-download"].download = state.file.path.split("/").pop();
    }
}

function applyFilter() {
    if (el["filter-input"].value === state.filter) return; // no change, no reconnect
    state.filter = el["filter-input"].value;
    connect();
}

function init() {
    MODES.forEach(function (m) { el["mode-select"].add(new Option(m, m)); });
    el["mode-select"].value = state.mode;
    el["mode-select"].onchange = function () { state.mode = el["mode-select"].value; connect(); };

    el["filter-input"].value = state.filter;
    el["filter-input"].addEventListener("keyup", function (e) { if (e.key === "Enter") applyFilter(); }); // Enter applies
    el["filter-input"].addEventListener("change", applyFilter); // and so does focus loss
    el["filter-apply"].onclick = applyFilter;

    // One delegated listener serves every per-line path prefix (logview.flush
    // tags each link with data-path instead of attaching a closure per line).
    el["logview"].addEventListener("click", function (e) {
        if (e.target.classList.contains("path-link")) jumpToFile(e.target.dataset.path);
    });

    el["file-select"].addEventListener("focus", refreshFiles);
    el["file-select"].onchange = function () {
        state.file = state.files[Number(el["file-select"].value)];
        updateDownload();
        syncModeOptions(); // an archive can only be grepped
        connect();
    };

    el["cfg-invert"].checked = state.invert;
    el["cfg-invert"].onchange = function () { state.invert = el["cfg-invert"].checked; connect(); };
    el["cfg-wrap"].checked = state.wrap;
    el["cfg-wrap"].onchange = function () { state.wrap = el["cfg-wrap"].checked; el["logview"].classList.toggle("wrap", state.wrap); };

    refreshFiles().then(function () { updateDownload(); connect(); });
}

init();
