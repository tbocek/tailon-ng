"use strict";

// Tailon-ng frontend: framework-free vanilla JavaScript. It fetches the file list
// and streams lines over Server-Sent Events. Modes: "tail" (follow) and "grep"
// (whole file), both with a browser-side regexp search that highlights
// matching lines; "find" greps server-side. Injected global: relativeRoot.

const RELATIVE_ROOT = (typeof relativeRoot !== "undefined" && relativeRoot) || "/";
// Demo mode (docs/demo.html, see make-demo.sh): no server at all — the only
// data source is files dragged in, which never leave the browser.
const DEMO = typeof window !== "undefined" && window.DEMO === true;
// "tail" follows live files; "find" searches the selection server-side for
// the first matches per file with context — bounded and fast on any file
// size; "find-all" (shown as "find (incl. arch.)") also searches rotated
// archives (.gz, .1, …), decoded server-side. "view" (wire value "grep") shows a whole file: it is also what
// clicking a find result or a line's file prefix opens. In tail and view the
// input is a browser-side search that highlights matches as you type without
// hiding lines (see searchApply). View works on single files only — for a
// group, a dump of several files interleaved is not useful, so the option is
// disabled (see syncModeOptions).
const MODES = [
    { value: "tail", label: "tail" },
    { value: "find", label: "find (excl. arch.)" },
    { value: "find-all", label: "find (incl. arch.)" },
    { value: "grep", label: "view" },
];
const TAIL_LINES = 10; // trailing lines shown when a tail starts
const MAX_LINES = 50000; // scrollback cap: lines kept here, and the most a view requests
const FIND_MAX = 3; // matches per file a find returns (findMaxMatches server-side)

const state = {
    files: [], file: null, mode: "tail", filter: "", wrap: false,
    source: null,
    prefix: "", // directory prefix shared by every served file, hidden in the UI
};

// Served files are append-only, so lines fetched once stay valid: single-file
// streams are cached per (file, mode) along with the byte offset read so far,
// and reconnecting re-renders the cache and asks the server only for what
// follows. Archives are immutable — once read, they never re-fetch. The
// server sends "event: reset" if a file shrank or was replaced.
const MAX_CACHE_VIEWS = 20;
const cache = new Map(); // key -> { lines: [], offset: -1, done: false }

function cacheEntry() {
    // Only tail and view (wire value "grep") reach here; find renders results,
    // not a line stream, and never caches. Streams are always unfiltered
    // (searching happens in the browser), so every search shares one copy.
    const key = JSON.stringify([state.file.path, state.mode]);
    let entry = cache.get(key);
    if (entry) cache.delete(key); // re-insert, so eviction drops the least recent
    else entry = { lines: [], offset: -1, done: false };
    cache.set(key, entry);
    while (cache.size > MAX_CACHE_VIEWS) cache.delete(cache.keys().next().value);
    return entry;
}

const el = {};
[
    "file-select", "mode-select", "filter-input", "filter-apply", "search-prev", "search-next", "search-count",
    "cfg-wrap", "action-download", "status", "scrollable", "logview", "toast", "loading",
].forEach(function (id) { el[id] = document.getElementById(id); });

// Line selection: clicking a line highlights it (clicking again clears it),
// ctrl+click toggles lines individually, shift+click selects the range from the
// last-clicked anchor. Ctrl-C copies just the highlighted lines. The DOM class
// "selected" is the source of truth; selAnchor is the range starting point.
let selAnchor = null;

let toastTimer = 0;
function toast(msg) {
    el["toast"].textContent = msg;
    el["toast"].classList.add("show");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { el["toast"].classList.remove("show"); }, 1800);
}

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
    anchor: null, // the #log-anchor spacer, kept as the log view's last element (see init)
    // append inserts rendered content above the anchor, which must stay last
    // so pinBottom has something exact to align to.
    append: function (node) { el["logview"].insertBefore(node, this.anchor); },
    // pinBottom glues the view to the end. Setting scrollTop = scrollHeight
    // relies on scrollHeight, which under content-visibility is partly
    // estimated from placeholder line heights — Firefox then lands mid-line,
    // clipping the last line. Scrolling the anchor into view forces exact
    // layout at the bottom instead.
    pinBottom: function () { this.anchor.scrollIntoView({ block: "end" }); },
    locate: null, // raw text to find, select and scroll to (set by jumpToFile)
    // While a grep loads, the view stays put — no per-frame bottom-sticking,
    // which costs a forced layout per frame on a large scrollback. A sweep
    // under the toolbar shows progress and EOF jumps to the bottom once,
    // unless the user started scrolling (reading) during the load.
    loading: false,
    userScrolled: false,
    clear: function () {
        if (this.raf) { cancelAnimationFrame(this.raf); this.raf = 0; }
        this.pending = [];
        this.locate = null;
        selAnchor = null;
        el["logview"].replaceChildren(this.anchor);
        this.lines = [];
        searchReset(); // the highlighted spans are gone with the rest
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
        if (this.raf) { cancelAnimationFrame(this.raf); this.raf = 0; } // also called directly at eof
        const scroll = !this.loading && this.atBottom();
        const frag = document.createDocumentFragment();
        let located = null;
        for (const ln of this.pending) {
            const span = document.createElement("span");
            span.className = "log-entry";
            if (ln.path) {
                const link = document.createElement("a");
                link.className = "path-link";
                link.textContent = stripPrefix(ln.path);
                link.title = "view " + ln.path;
                link.dataset.path = ln.path;
                span.appendChild(link);
                span.appendChild(document.createTextNode(": "));
            }
            appendAnsi(span, ln.text);
            // The line we were asked to jump to (compare with ANSI codes
            // stripped: the clicked line's text comes from the rendered DOM).
            if (this.locate !== null && ln.text.replace(ANSI_RE, "") === this.locate) {
                this.locate = null;
                located = span;
                span.classList.add("selected");
                selAnchor = span;
            }
            if (search.re) searchLine(span, search.re); // tail: match lines as they stream in
            frag.appendChild(span);
            this.lines.push(span);
        }
        this.pending = [];
        this.append(frag);
        if (this.lines.length > MAX_LINES + TRIM_CHUNK) {
            for (const old of this.lines.splice(0, this.lines.length - MAX_LINES)) old.remove();
            // If the range anchor was trimmed away, a later shift+click should
            // degrade to a plain click instead of silently doing nothing.
            if (selAnchor && !selAnchor.isConnected) selAnchor = null;
        }
        if (located) {
            located.scrollIntoView({ block: "center" });
            this.userScrolled = true; // a deliberate jump: EOF must not yank to the bottom
        } else if (scroll) {
            this.pinBottom();
        }
    },
};
logview.anchor = document.createElement("div");
logview.anchor.id = "log-anchor";
el["logview"].appendChild(logview.anchor);

function setStatus(text) { el["status"].textContent = text; el["status"].hidden = !text; }

// setLoading toggles the progress bar under the toolbar and (unless holdView
// is false — tail keeps bottom-following) the scroll-suppressed loading mode,
// see logview.loading. The bar starts as an indeterminate sweep and turns into
// a real 0-100 bar on the first "progress" event (views report byte progress —
// archives in compressed bytes); loads without byte progress (tail catch-up,
// find) keep the sweep.
function setLoading(on, holdView) {
    el["loading"].hidden = !on;
    el["loading"].classList.add("indeterminate");
    el["loading"].style.backgroundSize = "0% 100%";
    logview.loading = on && holdView !== false;
    if (on) logview.userScrolled = false;
}

// connect (re)opens the stream for the current file and mode. locate, when
// given, is the text of a line to select and center once rendered (set after
// the clear, which resets it — and before the cache replay, which may already
// contain it).
function connect(locate) {
    if (state.source) { state.source.close(); state.source = null; }
    if (!state.file) return;
    findSeq++; // any in-flight search result is for a view that no longer exists
    logview.clear();
    logview.locate = locate !== undefined ? locate : null;
    setLoading(false);
    setStatus("");

    // The one text input is the query in the find modes (the button runs it)
    // and a browser-side search everywhere else, highlighting matches as you
    // type without hiding anything (see searchApply) with ▲▼ steppers.
    // Make the split visible: search is browser-side over the shown lines,
    // find is a server-side scan of the whole files on disk.
    const finding = state.mode.indexOf("find") === 0;
    el["filter-input"].placeholder = finding ? "find in files (regexp)" : "search shown lines (regexp)";
    el["filter-input"].title = finding
        ? "Server-side: scans the whole selected files on disk and returns the first matches with context"
        : "Browser-side: searches the lines loaded in this view" +
        (state.mode === "grep" ? " — the counter also shows the whole-file total" : "");
    el["filter-apply"].hidden = !finding;
    el["search-prev"].hidden = el["search-next"].hidden = finding;

    // A dropped file lives in the browser: render it straight from memory —
    // no server involved (and in demo mode there is no server at all).
    if (state.file.local) {
        for (const t of state.file.lines) logview.write(null, t);
        logview.flush();
        viewSettled();
        return;
    }
    if (DEMO) {
        if (finding) {
            // find is a server-side scan and this demo has no backend; the
            // search box, in contrast, runs entirely in the browser.
            toast("find needs the server — this is a browser-only demo; use the search box instead");
        }
        setStatus("drop a log file here — it never leaves your browser");
        return;
    }

    if (finding) { findRequest(); return; }

    // nlines: tail's initial backlog — and in view mode the cap: anything past
    // the scrollback would be trimmed on arrival, so don't ask for it. Streams
    // are never filtered server-side (searching is browser-side), so every
    // search shares the one stream and the one cache entry.
    const p = new URLSearchParams({
        mode: state.mode,
        nlines: String(state.mode === "tail" ? TAIL_LINES : MAX_LINES),
    });

    let entry = null; // aggregate views are not cached: per-file offsets don't compose
    if (state.file.all) {
        p.set("all", "1");
        if (state.file.scope) p.set("scope", state.file.scope); // one subfolder only
    } else {
        p.set("path", state.file.path);
        entry = cacheEntry();
        for (const t of entry.lines) logview.write(null, t); // replay the cache (one batched flush)
        if (entry.done) { // a fully-read archive never grows: no request at all
            logview.flush();
            viewSettled();
            return;
        }
        if (entry.offset >= 0) p.set("offset", String(entry.offset));
        // A live single file in view mode loads its backlog and then keeps
        // following — new lines simply append, and the server reads only the
        // appended bytes from there on (files are append-only). Archives stay
        // a read-once (they never grow, and EOF marks their cache complete).
        if (state.mode === "grep" && !state.file.stale) p.set("mode", "tail");
    }

    setStatus("connecting…");
    // View loads end in EOF: hold the view still and show the bar until then.
    // Tail shows the bar just for its initial catch-up (the server says when
    // with a "live" event) and keeps its bottom-following throughout.
    if (state.mode !== "tail") setLoading(true);
    else setLoading(true, false);
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
    src.addEventListener("progress", function (e) {
        const p = JSON.parse(e.data); // {"d": bytes read, "t": bytes total}
        if (!(p.t > 0)) return;
        el["loading"].classList.remove("indeterminate");
        el["loading"].style.backgroundSize = Math.min(100, Math.round(p.d * 100 / p.t)) + "% 100%";
    });
    src.addEventListener("live", function () {
        if (state.mode === "grep") {
            // A live view: the backlog is rendered and the stream now follows.
            // Settle exactly like an EOF would — locate, search, landing.
            logview.flush();
            viewSettled();
            return;
        }
        setLoading(false); // tail's initial catch-up is rendered; now following
        searchApply(); // highlight the search over the backlog; new lines match as they arrive
    });
    src.addEventListener("eof", function () {
        if (entry && state.file && state.file.stale) entry.done = true; // archives are immutable
        logview.flush(); // render what's still queued before judging the jump target
        viewSettled();
        src.close(); state.source = null; setStatus("");
    });
    src.onerror = function () { setStatus("reconnecting…"); };
}

// Find mode: ask the server for the first matches per file (with context) and
// render them. findSeq guards against a stale response arriving after the user
// already switched to another view.
let findSeq = 0;
async function findRequest() {
    const seq = ++findSeq;
    if (!state.filter) { setStatus("enter a search (regexp)"); return; }
    setLoading(true);
    const p = new URLSearchParams({ q: state.filter });
    if (state.file.all) {
        p.set("all", "1");
        if (state.file.scope) p.set("scope", state.file.scope);
    } else {
        p.set("path", state.file.path);
    }
    if (state.mode === "find-all") p.set("stale", "1");

    let results = null;
    try {
        const resp = await fetch(RELATIVE_ROOT + "find?" + p.toString());
        if (!resp.ok) throw new Error(await resp.text());
        // NDJSON: {"d","t"} progress lines drive the 0-100 bar while the scan
        // runs (a rare needle means reading whole files); the last line
        // carries the results.
        const reader = resp.body.getReader();
        const dec = new TextDecoder();
        let buf = "";
        const takeLine = function (text) {
            if (!text) return;
            const obj = JSON.parse(text);
            if (obj.results) {
                results = obj.results;
            } else if (obj.t > 0 && seq === findSeq) {
                el["loading"].classList.remove("indeterminate");
                el["loading"].style.backgroundSize = Math.min(100, Math.round(obj.d * 100 / obj.t)) + "% 100%";
            }
        };
        for (; ;) {
            const chunk = await reader.read();
            if (chunk.done) break;
            buf += dec.decode(chunk.value, { stream: true });
            const parts = buf.split("\n");
            buf = parts.pop();
            parts.forEach(takeLine);
        }
        takeLine(buf.trim());
        if (results === null) throw new Error("incomplete response");
    } catch (err) {
        if (seq === findSeq) { setLoading(false); setStatus("search failed: " + err.message); }
        return;
    }
    if (seq !== findSeq) return; // another view took over while we waited
    setLoading(false);
    renderFind(results);
}

// renderFind shows each file's hits with dimmed context, one block per file
// separated by a rule. Clicking a file header opens the complete file with its
// first match selected; the result lines select and copy like any others.
function renderFind(results) {
    logview.clear();
    if (!results.length) { setStatus("no matches"); return; }
    let re = null; // the server validated the query; guard anyway
    try { re = new RegExp(state.filter, "g"); } catch (e) { }
    const frag = document.createDocumentFragment();
    const addLine = function (text, cls) {
        const span = document.createElement("span");
        span.className = "log-entry " + cls;
        appendAnsi(span, text);
        if (re) markText(span, re); // show what matched, wherever it appears
        frag.appendChild(span);
        logview.lines.push(span);
    };
    for (const f of results) {
        const n = f.matches.length;
        const head = document.createElement("div");
        head.className = "find-file";
        head.textContent = stripPrefix(f.path) + " — " + (n >= FIND_MAX ? FIND_MAX + "+" : n) + (n === 1 ? " match" : " matches");
        head.dataset.path = f.path;
        head.dataset.text = f.matches[0].text;
        head.title = "open " + f.path;
        frag.appendChild(head);
        f.matches.forEach(function (m, i) {
            if (i) {
                const gap = document.createElement("div");
                gap.className = "find-gap";
                gap.textContent = "···";
                frag.appendChild(gap);
            }
            for (const t of m.before) addLine(t, "ctx");
            addLine(m.text, "match");
            for (const t of m.after) addLine(t, "ctx");
        });
    }
    logview.append(frag);
    el["scrollable"].scrollTop = 0;
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

// Tail/view search: as you type (debounced), lines matching the regexp are
// highlighted in place — nothing is ever hidden — with the matched text
// wrapped in a <mark>. Enter / the ▲▼ buttons step through the matching
// lines; clearing the input clears the highlights. In tail, lines streaming
// in are matched as they render (see logview.flush).
//
// The pass over the scrollback runs in ~12ms slices (setTimeout between
// them), so a large buffer never blocks typing; a keystroke bumps search.gen,
// which cancels the in-flight pass before the debounce starts the next one.
// hits are the highlighted lines in order; stale holds lines whose highlights
// belong to a superseded query and are removed, sliced as well, at the start
// of the next pass. applied/re are the query the highlights were built from
// (re non-null only while a valid query is active).
const search = { hits: [], stale: [], cur: -1, applied: null, re: null, gen: 0, fileTotal: null };

// The browser can only search the lines it holds; in view mode the counter
// additionally shows the whole-file total, counted server-side (debounced —
// a full-file scan per keystroke would be wasteful on huge files).
let countTimer = 0, countSeq = 0;
function scheduleFileCount() {
    clearTimeout(countTimer);
    search.fileTotal = null;
    if (DEMO || state.mode !== "grep" || !state.file || state.file.all || state.file.local || !search.re) return;
    const seq = ++countSeq;
    const q = search.applied;
    const path = state.file.path;
    countTimer = setTimeout(async function () {
        let total = null;
        try {
            const p = new URLSearchParams({ q: q, count: "1", path: path });
            const resp = await fetch(RELATIVE_ROOT + "find?" + p.toString());
            if (!resp.ok) return;
            // NDJSON: skip the progress lines, take the counts line.
            for (const line of (await resp.text()).split("\n")) {
                if (!line) continue;
                const obj = JSON.parse(line);
                if (obj.counts) total = obj.counts.length ? obj.counts[0].n : 0;
            }
        } catch (e) { return; }
        if (seq !== countSeq || total === null) return;
        search.fileTotal = total;
        updateSearchCount();
    }, 600);
}

function searchableMode() { return state.mode === "tail" || state.mode === "grep"; }

function searchReset() {
    search.gen++; // cancels an in-flight pass; the DOM it worked on is gone
    search.hits = [];
    search.stale = [];
    search.cur = -1;
    search.applied = null;
    search.re = null;
    search.fileTotal = null;
    clearTimeout(countTimer);
    countSeq++;
    updateSearchCount();
}

function updateSearchCount() {
    const n = search.hits.length;
    el["search-count"].hidden = !searchableMode() || search.applied === null;
    let text = search.cur >= 0 ? (search.cur + 1) + "/" + n : n + (n === 1 ? " match" : " matches");
    // A windowed view must not hide how many matches exist beyond the lines
    // it holds: show the server-side whole-file total when it says more.
    if (search.fileTotal !== null && search.fileTotal > n) {
        text += " · " + search.fileTotal + " in file";
    }
    el["search-count"].textContent = text;
}

// unmark removes a line's <mark>s and merges its text nodes back together, so
// a later search sees the original uninterrupted text runs.
function unmark(entry) {
    for (const m of entry.querySelectorAll("mark")) {
        m.replaceWith(document.createTextNode(m.textContent));
    }
    entry.normalize();
}

// markText wraps every match of re inside entry's text in a <mark>. Matching
// is per text node (ANSI styling splits a line into several), so a match
// spanning two differently-styled runs stays unmarked — rare, and the line
// highlight still shows it.
function markText(entry, re) {
    const walker = document.createTreeWalker(entry, NodeFilter.SHOW_TEXT);
    const nodes = [];
    for (let n = walker.nextNode(); n; n = walker.nextNode()) nodes.push(n);
    for (const node of nodes) {
        const text = node.nodeValue;
        let m, last = 0, frag = null;
        re.lastIndex = 0;
        while ((m = re.exec(text)) !== null) {
            if (m[0] === "") { re.lastIndex++; continue; } // zero-width match: step past it
            if (!frag) frag = document.createDocumentFragment();
            frag.appendChild(document.createTextNode(text.slice(last, m.index)));
            const mark = document.createElement("mark");
            mark.textContent = m[0];
            frag.appendChild(mark);
            last = m.index + m[0].length;
        }
        if (frag) {
            frag.appendChild(document.createTextNode(text.slice(last)));
            node.replaceWith(frag);
        }
    }
}

// searchLine tests one rendered line and highlights it if it matches the
// active query. Already-highlighted lines are skipped, so calling it twice on
// a line (live pass racing the scrollback trim) is harmless.
function searchLine(entry, re) {
    if (entry.classList.contains("hit")) return;
    re.lastIndex = 0;
    if (!re.test(entry.textContent)) return;
    markText(entry, re);
    entry.classList.add("hit");
    search.hits.push(entry);
}

// searchApply (re)builds the search over the rendered lines: the previous
// highlights are removed first (so clearing the query deselects everything,
// and editing it keeps just the lines that still match), then every line is
// tested and marked. Both phases yield after ~12ms and resume on a timeout,
// abandoning the pass the moment search.gen moves on. done, when given, runs
// once the pass completes (never for an abandoned pass).
function searchApply(done) {
    const gen = ++search.gen;
    search.stale = search.stale.concat(search.hits); // now-stale highlights to undo
    search.hits = [];
    search.cur = -1;
    search.applied = null;
    search.re = null;
    if (searchableMode() && state.filter) {
        let re = null;
        try { re = new RegExp(state.filter, "g"); } catch (e) { /* incomplete regexp while typing */ }
        if (re) {
            search.applied = state.filter;
            search.re = re;
        }
    }
    scheduleFileCount(); // view mode: the whole-file total, counted server-side
    let i = 0;
    const step = function () {
        if (gen !== search.gen) return; // superseded: the newer pass owns the cleanup
        const t0 = performance.now();
        while (search.stale.length) {
            const s = search.stale.pop();
            s.classList.remove("hit", "current");
            unmark(s);
            if (performance.now() - t0 > 12) { setTimeout(step, 0); return; }
        }
        // The live array, on purpose: lines that stream in mid-pass get
        // scanned too. A concurrent scrollback trim may shift indexes — a
        // re-scanned line is caught by searchLine's already-marked check, a
        // skipped one by the next pass.
        while (search.re && i < logview.lines.length) {
            searchLine(logview.lines[i++], search.re);
            if (performance.now() - t0 > 12) { updateSearchCount(); setTimeout(step, 0); return; }
        }
        updateSearchCount();
        if (done) done();
    };
    step();
}

// searchStep moves the current match by dir (±1, wrapping) and centers it.
function searchStep(dir) {
    if (search.applied !== state.filter) searchApply(); // Enter right after typing: apply now
    // The scrollback trim may have dropped hits from the DOM; prune them.
    if (search.hits.some(function (s) { return !s.isConnected; })) {
        const cur = search.cur >= 0 ? search.hits[search.cur] : null;
        search.hits = search.hits.filter(function (s) { return s.isConnected; });
        search.cur = cur ? search.hits.indexOf(cur) : -1;
    }
    if (!search.hits.length) return;
    if (search.cur >= 0) search.hits[search.cur].classList.remove("current");
    search.cur = search.cur < 0
        ? (dir > 0 ? 0 : search.hits.length - 1)
        : (search.cur + dir + search.hits.length) % search.hits.length;
    const s = search.hits[search.cur];
    s.classList.add("current");
    s.scrollIntoView({ block: "center" });
    logview.userScrolled = true; // a deliberate jump: EOF must not yank to the bottom
    updateSearchCount();
}

// viewSettled runs once a view's content is fully rendered (at eof, or right
// after a fully-cached archive replays): it reports a jump target that never
// appeared, rebuilds the search highlights, and decides where to land — on
// the clicked line, else on the first match, else at the bottom.
function viewSettled() {
    if (logview.locate !== null) {
        // The whole file rendered and the jump target never appeared
        // (rotated away, or past the view's line cap).
        logview.locate = null;
        toast("line not found");
    }
    searchApply(function () {
        const i = selAnchor ? search.hits.indexOf(selAnchor) : -1;
        if (i >= 0) {
            search.cur = i; // ▲▼ step onward from the clicked line
            search.hits[i].classList.add("current");
            updateSearchCount();
        } else if (search.hits.length && !logview.userScrolled) {
            searchStep(1); // no (surviving) target: land on the first match
        } else if (!logview.userScrolled) {
            logview.pinBottom(); // no search at all: the end is the news
        }
    });
    setLoading(false);
}

// jumpToFile selects the file in the dropdown and views it whole (used by the
// clickable per-line path prefix in multi-file streams). When the clicked
// line's text is given, the view scrolls to that line and highlights it.
function jumpToFile(path, text) {
    const i = state.files.findIndex(function (f) { return !f.all && f.path === path; });
    if (i < 0) return;
    el["file-select"].value = String(i);
    state.file = state.files[i];
    state.mode = "grep"; // "view": the complete file (syncModeOptions syncs the select)
    // Keep the query: a view hides nothing, so a find's search carries over
    // and its matches arrive already highlighted (viewSettled lands on the
    // clicked line, or the first match when it is gone).
    updateDownload();
    syncModeOptions();
    // Normalize the jump target to rendered form: find results carry raw text
    // whose ANSI escapes the view strips, and locate compares stripped
    // against stripped.
    connect(text !== undefined ? text.replace(ANSI_RE, "") : null);
}

// syncModeOptions matches the mode options to the selected entry: "tail" is
// disabled for a rotated/compressed file (it will never grow), which is viewed
// instead, and "view" is disabled for group entries (a whole-file dump of many
// files interleaved is not useful — tail or find them instead), which fall
// back to tail. It only adjusts state — the caller connects.
function syncModeOptions() {
    const stale = state.file && state.file.stale; // groups are never stale
    const group = state.file && state.file.all;
    const local = state.file && state.file.local; // dropped file: view-only
    el["mode-select"].options[0].disabled = !!stale || !!local; // options[0] is "tail"
    el["mode-select"].options[1].disabled = !!local || DEMO; // "find" and "find-all" are
    el["mode-select"].options[2].disabled = !!local || DEMO; // server-side — use the search box
    el["mode-select"].options[3].disabled = !!group; // options[3] is "view"
    if (DEMO && state.mode.indexOf("find") === 0) state.mode = "grep";
    if (local) state.mode = "grep";
    if (stale && state.mode === "tail") state.mode = "grep";
    if (group && state.mode === "grep") state.mode = "tail";
    el["mode-select"].value = state.mode;
}

// Files dragged onto the page: read and rendered entirely in the browser —
// they are never uploaded. Each becomes a "local" entry in the file selector;
// .gz files are decoded with the browser's own DecompressionStream.
const localFiles = [];
async function addLocalFile(file) {
    let stream = file.stream();
    if (/\.gz$/i.test(file.name) && typeof DecompressionStream !== "undefined") {
        stream = stream.pipeThrough(new DecompressionStream("gzip"));
    }
    const lines = [];
    let buf = "";
    const push = function (l) {
        lines.push(l);
        if (lines.length > MAX_LINES + TRIM_CHUNK) lines.splice(0, lines.length - MAX_LINES);
    };
    try {
        const reader = stream.getReader();
        const dec = new TextDecoder();
        for (; ;) {
            const chunk = await reader.read();
            if (chunk.done) break;
            buf += dec.decode(chunk.value, { stream: true });
            const parts = buf.split(/\r\n|[\r\n]/); // LF, CRLF and CR-only files
            buf = parts.pop();
            parts.forEach(push);
        }
    } catch (err) {
        toast("cannot read " + file.name + ": " + err.message);
        return;
    }
    if (buf) push(buf);

    const entry = { path: file.name, local: true, lines: lines };
    const i = localFiles.findIndex(function (f) { return f.path === file.name; });
    if (i >= 0) localFiles[i] = entry; else localFiles.push(entry);

    await refreshFiles();
    const j = state.files.indexOf(entry);
    if (j < 0) return;
    el["file-select"].value = String(j);
    state.file = entry;
    updateDownload();
    syncModeOptions(); // local files are view-only
    connect();
    toast(file.name + " — " + lines.length + " lines, local only");
}

async function refreshFiles() {
    let data;
    if (DEMO) data = []; // no server: dropped files are the only entries
    else {
        try { data = await (await fetch(RELATIVE_ROOT + "list")).json(); }
        catch (e) { setStatus("could not load file list"); return; }
    }

    const prev = state.file && (state.file.scope || state.file.path);
    state.files = [];
    state.prefix = commonPrefix(data.map(function (e) { return e.path; }));
    el["file-select"].replaceChildren();

    el["file-select"].add(new Option("All files", "0"));
    state.files.push({ path: "", all: true });

    // Offer each subfolder as a "tail/find everything under here" entry. A folder
    // is any ancestor directory holding some — but not all — of the files; one
    // holding all of them would just duplicate "All files", so it is skipped.
    const groups = [];
    const counts = {};
    data.forEach(function (entry) {
        let d = entry.path;
        for (let i = d.lastIndexOf("/"); i > 0; i = d.lastIndexOf("/")) {
            d = d.slice(0, i);
            counts[d] = (counts[d] || 0) + 1;
        }
    });
    Object.keys(counts).filter(function (d) { return counts[d] < data.length; })
        .forEach(function (d) { groups.push({ path: d, scope: d + "/", all: true, dir: true }); });

    // Also group files sharing a name prefix (cut at . - _), e.g. two hosts
    // logging as 192.168.1.5.log and 192.168.1.22.log yield "▸ 192.168.1*",
    // selectable for tail and find like a folder. Only maximal prefixes
    // matching ≥2 files are offered, and none that just mirror a directory.
    const dirTotals = {};
    data.forEach(function (e) {
        const dir = e.path.slice(0, e.path.lastIndexOf("/") + 1);
        dirTotals[dir] = (dirTotals[dir] || 0) + 1;
    });
    const nameGroups = {};
    data.forEach(function (e) {
        const dir = e.path.slice(0, e.path.lastIndexOf("/") + 1);
        const base = e.path.slice(dir.length);
        for (let i = 1; i < base.length; i++) {
            if (".-_".indexOf(base[i]) >= 0) {
                const p = dir + base.slice(0, i);
                nameGroups[p] = (nameGroups[p] || 0) + 1;
            }
        }
    });
    Object.keys(nameGroups).filter(function (p) {
        const dir = p.slice(0, p.lastIndexOf("/") + 1);
        if (nameGroups[p] < 2 || nameGroups[p] === dirTotals[dir]) return false;
        // Keep only the longest prefix naming this same group of files.
        return !Object.keys(nameGroups).some(function (q) {
            return q !== p && q.indexOf(p) === 0 && nameGroups[q] === nameGroups[p];
        });
    }).forEach(function (p) { groups.push({ path: p, scope: p, all: true }); });

    // Render groups and files as one tree: sorted by path, each entry nests
    // under the closest group whose scope string-prefixes it — the same rule
    // the server scopes by, so the display and the selection always agree —
    // and is indented by its depth. Labels are relative to the parent: a
    // folder shows its own segment, a name group its base-name prefix, a file
    // its base name. Every ancestor folder below the common prefix is offered,
    // so a file's base name is never ambiguous.
    const stack = []; // scopes of the groups enclosing the current entry
    groups.concat(data).sort(function (a, b) {
        const ka = a.scope || a.path, kb = b.scope || b.path;
        if (ka !== kb) return ka < kb ? -1 : 1;
        return (a.all ? 0 : 1) - (b.all ? 0 : 1); // a group precedes a file of the same name
    }).forEach(function (en) {
        const key = en.scope || en.path;
        while (stack.length && key.indexOf(stack[stack.length - 1]) !== 0) stack.pop();
        let label;
        if (en.dir) {
            let parent = state.prefix; // the closest enclosing *folder* shown
            for (let i = stack.length - 1; i >= 0; i--) {
                if (stack[i].slice(-1) === "/") { parent = stack[i]; break; }
            }
            label = "▸ " + key.slice(parent.length);
        } else if (en.all) {
            label = "▸ " + en.path.slice(en.path.lastIndexOf("/") + 1) + "*";
        } else {
            label = en.path.slice(en.path.lastIndexOf("/") + 1) + (en.stale ? "  (archived)" : "");
        }
        const indent = "\u00A0\u00A0\u00A0".repeat(stack.length); // nbsp: <option> would collapse plain spaces
        el["file-select"].add(new Option(indent + label, String(state.files.length)));
        state.files.push(en);
        if (en.all) stack.push(en.scope);
    });

    // Dropped files close the list, visibly local.
    localFiles.forEach(function (f) {
        el["file-select"].add(new Option("⬇ " + f.path + " (local)", String(state.files.length)));
        state.files.push(f);
    });

    // Restore the previous selection by path/scope, else select the first entry.
    let i = state.files.findIndex(function (f) { return (f.scope || f.path) === prev; });
    if (i < 0) i = state.files.length ? 0 : -1;
    state.file = i >= 0 ? state.files[i] : null;
    if (i >= 0) el["file-select"].value = String(i);
    syncModeOptions();
}

function updateDownload() {
    const off = !state.file || state.file.all || state.file.local;
    el["action-download"].hidden = off;
    if (!off) {
        el["action-download"].href = RELATIVE_ROOT + "files/?path=" + encodeURIComponent(state.file.path);
        el["action-download"].download = state.file.path.split("/").pop();
    }
}

// applyFilter runs a find (state.filter tracks the input on every keystroke).
// Tail and view never route here — their input searches as you type.
function applyFilter() {
    connect(); // even unchanged: Enter or the button means "search now"
}

function init() {
    MODES.forEach(function (m) { el["mode-select"].add(new Option(m.label, m.value)); });
    el["mode-select"].value = state.mode;
    el["mode-select"].onchange = function () { state.mode = el["mode-select"].value; connect(); };

    el["filter-input"].value = state.filter;
    // Tail and view search as you type, debounced; a keystroke immediately
    // cancels the in-flight highlight pass (search.gen) so typing stays
    // responsive on a large scrollback. Enter steps to the next match
    // (Shift+Enter to the previous), like a browser's find. Find keeps
    // deliberate application: Enter or the button.
    let searchTimer = 0;
    el["filter-input"].addEventListener("input", function () {
        state.filter = el["filter-input"].value;
        if (!searchableMode()) return; // find applies deliberately, on Enter or the button
        search.gen++; // cancel the in-flight pass right away
        clearTimeout(searchTimer);
        searchTimer = setTimeout(searchApply, 300);
    });
    el["filter-input"].addEventListener("keyup", function (e) {
        if (e.key !== "Enter") return;
        if (searchableMode()) {
            clearTimeout(searchTimer);
            searchStep(e.shiftKey ? -1 : 1); // re-applies first if the text changed
        } else {
            applyFilter();
        }
    });
    el["filter-apply"].onclick = applyFilter;
    el["search-prev"].onclick = function () { searchStep(-1); };
    el["search-next"].onclick = function () { searchStep(1); };

    // One delegated listener serves every line (logview.flush attaches no
    // per-line handlers). A plain click on the path prefix jumps to grepping
    // that file — carrying the line's text so the grep view scrolls to it; with
    // a modifier held it selects instead (jumping mid-range-select would jar).
    // Clicks select: plain click highlights just that line (again to clear),
    // ctrl+click toggles lines individually, shift+click selects the range from
    // the last-clicked line (or acts as a plain click when there is no anchor).
    // A drag that selected text is not a line click, and clicking the empty
    // space below the lines clears the selection.
    el["scrollable"].addEventListener("click", function (e) {
        const plain = !e.shiftKey && !e.ctrlKey && !e.metaKey;
        if (plain && e.target.classList.contains("path-link")) {
            const entry = e.target.parentNode;
            // The raw line text follows the "prefix: " nodes.
            const text = entry.textContent.slice(e.target.textContent.length + 2);
            jumpToFile(e.target.dataset.path, text);
            return;
        }
        const head = plain && e.target.closest(".find-file");
        if (head) {
            jumpToFile(head.dataset.path, head.dataset.text); // the complete file, first match selected
            return;
        }
        if (!window.getSelection().isCollapsed) return; // text drag-select, not a line click
        const clearAll = function () {
            for (const s of el["logview"].querySelectorAll(".log-entry.selected")) {
                s.classList.remove("selected");
            }
        };
        const entry = e.target.closest(".log-entry");
        if (!entry) { clearAll(); selAnchor = null; return; } // click-away deselects
        if (e.shiftKey && selAnchor && selAnchor.isConnected) {
            const a = logview.lines.indexOf(selAnchor), b = logview.lines.indexOf(entry);
            for (let i = Math.min(a, b); i <= Math.max(a, b) && i >= 0; i++) {
                logview.lines[i].classList.add("selected");
            }
        } else if (plain && entry.classList.contains("selected")) {
            clearAll(); // clicking a highlighted line unhighlights
        } else {
            if (plain) clearAll();
            entry.classList.toggle("selected"); // plain: select this one; ctrl: toggle
            selAnchor = entry.classList.contains("selected") ? entry : selAnchor;
        }
    });
    // Drag a log file anywhere onto the page to view it — read locally,
    // never uploaded (in the demo this is the only data source).
    document.addEventListener("dragover", function (e) {
        e.preventDefault();
        document.body.classList.add("dragging");
    });
    document.addEventListener("dragleave", function () {
        document.body.classList.remove("dragging");
    });
    document.addEventListener("drop", function (e) {
        e.preventDefault();
        document.body.classList.remove("dragging");
        for (const f of e.dataTransfer.files) addLocalFile(f);
    });

    // Shift+click must not trigger the browser's native text selection.
    el["scrollable"].addEventListener("mousedown", function (e) {
        if (e.shiftKey) e.preventDefault();
    });
    // Nothing scrolls programmatically during a load, so any scroll (past the
    // clamp-to-0 that clearing the view fires) means the user started reading.
    el["scrollable"].addEventListener("scroll", function () {
        if (logview.loading && el["scrollable"].scrollTop > 0) logview.userScrolled = true;
    });

    // Ctrl-C with highlighted lines copies exactly those lines — unless the
    // user drag-selected text just now (the more recent, explicit intent) or is
    // in an input. Escape clears the selection.
    document.addEventListener("keydown", function (e) {
        if (e.key === "Escape") {
            for (const s of el["logview"].querySelectorAll(".log-entry.selected")) {
                s.classList.remove("selected");
            }
            selAnchor = null;
            return;
        }
        if (!(e.ctrlKey || e.metaKey) || e.key !== "c") return;
        if (/^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)) return;
        if (!window.getSelection().isCollapsed) return; // native copy of dragged text
        const sel = el["logview"].querySelectorAll(".log-entry.selected");
        if (!sel.length) return; // no highlights: native copy
        e.preventDefault();
        const text = Array.from(sel).map(function (s) { return s.textContent; }).join("\n");
        navigator.clipboard.writeText(text).then(
            function () { toast(sel.length + (sel.length === 1 ? " line copied" : " lines copied")); },
            function () { toast("copy failed"); }
        );
    });

    el["file-select"].addEventListener("focus", refreshFiles);
    el["file-select"].onchange = function () {
        state.file = state.files[Number(el["file-select"].value)];
        updateDownload();
        syncModeOptions(); // an archive can only be grepped
        connect();
    };

    el["cfg-wrap"].checked = state.wrap;
    el["cfg-wrap"].onchange = function () { state.wrap = el["cfg-wrap"].checked; el["logview"].classList.toggle("wrap", state.wrap); };

    refreshFiles().then(function () { updateDownload(); connect(); });
}

init();
