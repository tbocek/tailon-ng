<p align="center">
  <img src="docs/scr.png" alt="Tailon-ng screenshot" width="900">
</p>

# Tailon-ng

[![Apache License](https://img.shields.io/badge/license-Apache-blue.svg)](https://github.com/tbocek/tailon-ng/blob/main/LICENSE)

> **This is a fork of [gvalkov/tailon].** It keeps the purpose — tail and grep your
> log files from the browser — but rebuilds the project around the Go standard
> library: **two third-party dependencies** (pure-Go xz/zstd decoders for rotated
> archives, nothing else), no JavaScript toolchain, and a single static binary.

**Do one thing, but do it right.** Tailon-ng tails and shows your log files
from the browser. Thats it. There is no dashboard builder, no ingestion pipeline, 
no query language, no agents and no config file to learn. There is only: live tailing
that arrives in milliseconds, a bounded server-side find that stays fast on
huge files and rotated archives, an append-aware cache that makes switching
views instant, logs from many hosts merged in timestamp order, and a UI with editor-style search toggles, real progress bars and ANSI colors.

Reading, following and searching are all done natively in Go and in the browser: 
tailon-ng never shells out to `tail`, `grep` or any other tool. It is almost entirely 
Go standard library, the only third-party code is two pure-Go decompression libraries 
([ulikunitz/xz] and [klauspost/compress] for zstd) used to read rotated archives, 
and ships as a single static binary.

tailon-ng has **no built-in authentication**. Anyone who can reach its port can read 
the files it serves, so bind it to localhost (`-b 127.0.0.1:8080`) or put it behind 
an authenticating reverse proxy.

## How this fork differs from the original

The "original" here is **[gvalkov/tailon]**, which itself is a fork of [gvalkov/tailon-legacy]. Comparison:

| Area | [gvalkov/tailon] (upstream) | This fork |
| --- | --- | --- |
| Third-party dependencies | Vue.js, SockJS, build-time tooling | **Two** pure-Go decompression libraries (xz, zstd); everything else is standard library |
| Frontend | Vue.js single-page app | Framework-free vanilla HTML/CSS/JS |
| Live updates | SockJS | Server-Sent Events |
| Frontend assets | produced by a code-generation/build step | embedded with `//go:embed`, no build step |
| Releases | GoReleaser | a small [`release.sh`](release.sh) + GitHub Actions |
| Tests | Python integration tests | Go unit tests (`go test ./...`) |

## Install

Install the latest release binary for your OS and architecture (Linux and
macOS, Intel and Apple Silicon). The script installs to `/usr/local/bin`, or
`~/.local/bin` when that isn't writable:

```
curl -sL https://raw.githubusercontent.com/tbocek/tailon-ng/main/install.sh | bash
```

Or install from source with Go (1.26+):

```
go install github.com/tbocek/tailon-ng@latest
```

Prebuilt binaries are also attached to every entry on the [releases] page.

### Docker

A multi-arch (amd64/arm64) image is published to the GitHub Container Registry
on every release. Paths to serve are passed as arguments, exactly as on the
command line:

```
docker run --rm -p 127.0.0.1:8080:8080 -v /var/log:/var/log:ro \
  ghcr.io/tbocek/tailon-ng:latest /var/log
```

`:latest` is the newest release; `:N` (e.g. `:28`) pins one — image tags mirror
the plain-increment git tags (v1, v2, …). The image is
built `FROM` [distroless] `static` — just the static binary, no shell or
package manager. It runs as root so it can read root-owned system logs (such as
`/var/log/syslog`) out of the box; to drop privileges when you only serve logs
a non-root user can read, add `--user` (for example `--user 65532`, or
`--user 65532:4` to keep the owning group's read access).

The image is small. A pull downloads the compressed content, about **4.33 MB**; 
the disk-usage figure is the unpacked image (both architectures, under the 
containerd image store):

```
IMAGE                         ID             DISK USAGE   CONTENT SIZE   EXTRA
ghcr.io/tbocek/tailon-ng:28   34285fe33ef8       19.3MB         4.33MB
```

## Usage

Run tailon-ng with the files or directories you want to watch — each argument
is a file, a directory (served recursively; new files are picked up as they
appear), or a shell glob, where `*` matches within a directory and `**`
across them — then open http://localhost:8080:

```
tailon-ng /var/log/apache/access.log /var/log/apache/error.log /var/log/messages
tailon-ng /var/log/apache/ /var/log/nginx/
tailon-ng "/var/log/**.log"
```

The server itself is configured through its command-line flags, everything else 
is the web UI.

**Three modes.** **tail** follows live, like `tail -f` — the mode for groups
and **All files**, whose streams merge in timestamp order. **view** shows a
whole file; selecting a single file always opens it in view, and a view keeps
following after its backlog loads, so for one file view *is* tail plus
history. **find** searches on the server and shows the first matches from
each file with a few lines of context; two selects adjust how many matches
(up to 100 per file) and how much context (up to ±10 lines). Clicking a
result opens the file's view at that line, with the query carried along and
its matches already highlighted.

**Search vs find.** In tail and view, the input is a browser-side **search**
that highlights and hides nothing: matching lines and text light up as you
type, lines streaming in live match as they arrive, and Enter or the ▲▼
buttons step between matches. The split is labeled in the UI: search covers
the shown lines, **find** scans the whole files on the server. A view's match
counter bridges the two — it also shows the server-counted whole-file total,
and clicking it continues the search on the server, listing up to 100 matches
across the whole file, each jumping back into the view at its line.

Three editor-style toggles inside the input shape search and find alike:
**Aa** match case, **.\*** regular expression (off searches the literal
text), **!** invert (keep the lines that do *not* match, like `grep -v`). In
find mode a fourth, **gz**, widens the find to rotated archives, decoded
transparently. The **☰ menu** holds the remaining settings — **wrap lines**
and **live view** (off makes a view read once) — plus the running version.
All toggles and settings persist in the browser (localStorage).

**The file selector is a tree — and writable.** Subfolders, and groups of
files sharing a name prefix (per-host logs, say), are selectable entries —
tail or find everything under one of them. Typing into the selector filters
the entries, prefix-matching each path segment (`ngi` finds `nginx/` anywhere
in the tree); ArrowUp/Down and Enter select. Rotation leftovers (`.gz`, `.bz2`, `.xz`,
`.zst`, numeric `.1`, date-stamped `-YYYYMMDD`, `.old`, `.bak`) never get
appended, so the selector does not list them — the **gz** toggle widens a
find to them, and clicking a result opens the decoded view. Binary
files (a NUL byte in the first kilobyte — `wtmp`, journald files, stray
executables) are not served at all.

**Merged streams stay readable.** In **All files** (the default) every line
carries its file as a prefix in a stable per-file color, so interleaved
sources are told apart at a glance — and clicking a prefix jumps into that
file's view, scrolled to that very line. The merge recognizes the common
timestamp formats (ISO 8601 / RFC 3339, `YYYY-MM-DD HH:MM:SS`, slash dates,
Apache/CLF, Unix `ctime`, syslog) at or near the start of a line; a line
without one keeps its file's previous timestamp, so multi-line entries stay
together. Timestamps only order the merge — they are never added to the
display.

**Lines are interactive.** Clicking selects a line, **Ctrl+click** toggles
lines individually, **Shift+click** selects a range, **Escape** clears — and
**Ctrl-C** copies exactly the highlighted lines, for pasting a few relevant
entries into an issue or a chat. A normal mouse drag still selects and copies
text natively.

**Fast by design.** On Linux, appended lines reach the browser in
milliseconds via inotify (with a polling fallback elsewhere). Log files are
append-only, and the frontend exploits that: every single-file view is cached
with the byte offset read so far, so switching files or modes re-renders
instantly and fetches only what arrived since — a fully-read archive is never
requested again, and a rotated or truncated file resets its view. A view
never ships more than the 50,000 lines the scrollback keeps, and big loads
drive a real 0–100 progress bar under the toolbar.

### Example: central syslog server

A common deployment is a host that aggregates logs from many machines via
[syslog-ng] (or rsyslog) into a directory tree such as `/var/log/remote`. Point
tailon-ng at the directory to serve everything under it recursively:

```
tailon-ng /var/log/remote/
```

Every file beneath it — including per-host subdirectories — shows up in the file
picker, and **All files** streams them all merged in timestamp order.

Tailon-ng's server-side functionality is summarized entirely in its help message:

[//]: # (run "./tailon-ng --help" to update the next section)

[//]: # (BEGIN HELP_USAGE)
```
Usage: tailon-ng [options] <path> [<path> ...]

Tailon-ng is a webapp for searching through log files from your browser.

  -b, --bind string            Address and port to listen on (default ":8080")
  -h, --help                   Show this help message and exit
  -r, --relative-root string   Webapp relative root (default "/")

Tailon-ng is configured entirely through command-line flags.

Each <path> is a file, a directory, or a shell glob, where "*" matches within a
directory and "**" across them (so "/var/log/**.log" finds .log files at any
depth). Directories are served recursively, and new files are picked up as they
appear. Several paths can be given as separate arguments.

Rotation leftovers (.gz, .bz2, .xz, .zst, .1, -YYYYMMDD, .old, .bak) never get
appended, so they stay hidden until the "gz" toggle (web UI, in find
mode's search input) turns them on: the file selector then lists them, and find searches
them too, decompressed transparently.

On Linux, appended lines are pushed instantly via inotify; elsewhere, and on
filesystems without notification support, tailon-ng falls back to polling.

Example usage:
  tailon-ng /var/log/syslog /var/log/auth.log
  tailon-ng /var/log/nginx/ /var/log/apache/
  tailon-ng /var/log/remote/
  tailon-ng "/var/log/**.log"
  tailon-ng -b 127.0.0.1:8080 /var/log/messages
```
[//]: # (END HELP_USAGE)

## Security

**No built-in authentication.** By default tailon-ng is reachable by anyone who
can connect to its address and port, and it serves — and lets clients download —
exactly the files you point it at. Bind it to localhost (`-b 127.0.0.1:8080`) or
put it behind an authenticating reverse proxy. It is a log viewer, not a gateway.

**No shell, no external commands.** Tailon-ng never runs `tail`, `grep`, `sed`,
`awk` or any subprocess; reading, following and find are all done in-process in
Go (search-as-you-type runs in the browser). The find query is a Go ([RE2])
regular expression, so nothing entered in the browser can cause shell or
command injection.

**Safe downloads.** Files are served as plain-text attachments with
`X-Content-Type-Options: nosniff`, so a log line that happens to look like HTML
can't be rendered as script in your browser.

## Development

### Frontend

The frontend is plain, framework-free HTML, CSS and JavaScript: three flat files
in `frontend/` (one Go template `main.html`, `main.css`, `main.js`), embedded
into the binary at compile time with `//go:embed` (see `frontend.go`). The
favicon is an inline SVG data URI in `main.html` — no image files at all. There is no build
step or toolchain — edit the files directly and rebuild the binary. The UI
talks to the backend over Server-Sent Events.

### Backend

The backend is written in straightforward Go, almost entirely standard library:
flag parsing, configuration, HTTP serving, access logging, file following,
server-side find and gzip/bzip2 decoding for the Server-Sent Events stream. The
only third-party dependencies are [ulikunitz/xz] and [klauspost/compress],
which decode `.xz` and `.zst` archives. File reading and following live in
`tailer.go`; the inotify wake-up path (Linux, raw `syscall` — no dependency) in
`watcher_linux.go`, with the polling fallback in `watcher_other.go`.

### Versioning

Verifiable release binaries are stamped with their git tag at build time
(`-ldflags "-X main.version=..."` in the GitHub Actions workflow), which is what
the UI's version badge shows. Local builds show `dev`; to stamp one yourself:

```
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
```

## Project lineage

Three distinct projects have carried the **tailon** name; this is the third, and
the source of the recurring "wait, which tailon?" confusion:

1. **[tailon-legacy]** — the first one, written in Python + Tornado with a custom
   TypeScript frontend.
2. **[gvalkov/tailon]** — a full rewrite in Go with a Vue.js + SockJS frontend,
   configured through a file and released with GoReleaser. **This is the upstream
   this repository is forked from.**
3. **This fork (tailon-ng)** — drops the third-party frontend and tooling for a
   framework-free, nearly dependency-free, single static binary. See [How this fork
   differs from the original](#how-this-fork-differs-from-the-original) for the
   point-by-point comparison.

## AI Usage

I view AI LLMs as a tool to help write faster and better code. AI assistants
(Opus/Fable/Qwen/Gemma) wrote a substantial part of this tool. I question the 
code piece by piece, the assistant explains, simplifies or fixes. The design 
calls and the final review stay with me, and every change is backed by 
tests (`go test -race ./...`). This tool is currently used in production.