<p align="center">
  <img src="docs/scr.png" alt="Tailon screenshot" width="900">
</p>

# Tailon

[![Go Report Card](https://goreportcard.com/badge/github.com/tbocek/tailon)](https://goreportcard.com/report/github.com/tbocek/tailon)
[![Apache License](https://img.shields.io/badge/license-Apache-blue.svg)](https://github.com/tbocek/tailon/blob/main/LICENSE)

> **This is a fork of [gvalkov/tailon].** It modernizes the project and strips it
> down to **zero third-party dependencies**: the frontend assets are embedded with
> Go's `embed` package (no code generation), the web UI was rewritten as
> framework-free vanilla HTML/CSS/JavaScript that streams over **Server-Sent
> Events** (replacing the Vue.js + SockJS frontend), the config file was dropped
> in favor of command-line flags, the Python integration tests were removed, and
> releases are produced by a small [`release.sh`](release.sh) plus GitHub Actions
> instead of GoReleaser.

Tailon is a webapp for looking at and searching through log files from your
browser. It serves files — single files, globs or whole directories — and lets
you **tail** them live or **grep** through them, with a regular-expression
filter (which can be inverted). Reading, following and filtering are all done
natively in Go: tailon never shells out to `tail`, `grep` or any other tool, and
it has **no dependencies** — just the Go standard library, shipped as a single
static binary.

## Install

Install the latest release binary for your OS and architecture (Linux and
macOS, Intel and Apple Silicon). The script installs to `/usr/local/bin`, or
`~/.local/bin` when that isn't writable:

```
curl -sL https://raw.githubusercontent.com/tbocek/tailon/main/install.sh | bash
```

Or install from source with Go (1.26+):

```
go install github.com/tbocek/tailon@latest
```

Prebuilt binaries are also attached to every entry on the [releases] page.

## Usage

Each file can be viewed in two modes — **tail** (follow the file live, like
`tail -f`) or **grep** (read the whole file from the start) — and narrowed with
an optional regular-expression **filter** that can be inverted (both set in the
UI). Tailon itself is configured entirely with command-line flags.

To get started, run tailon with the list of files that you wish to monitor.

```
tailon /var/log/apache/access.log /var/log/apache/error.log /var/log/messages
```

Tailon can serve single files, globs or whole directory trees.

The web UI's file selector includes an **All files** entry (selected by default)
that streams every served file at once, each line prefixed by its file and the
streams **merged in timestamp order**. Several common formats are recognized at
(or near) the start of each line — ISO 8601 / RFC 3339, `YYYY-MM-DD HH:MM:SS`,
slash-separated dates, Apache/CLF, Unix `ctime` and syslog (RFC 3164). The format
is detected per file from its first lines (not guessed from a single one), and a
line without a recognizable timestamp keeps its file's previous one, so
multi-line entries stay together. Handy when you're watching logs from many
hosts together.

### Example: central syslog server

A common deployment is a host that aggregates logs from many machines via
[syslog-ng] (or rsyslog) into a directory tree such as `/var/log/remote`. Point
tailon at the directory to serve everything under it recursively:

```
tailon /var/log/remote/
```

Or use a glob with `group=`/`alias=` to organize the file picker by host:

```
tailon "group=hosts,/var/log/remote/*/*.log"
```

Tailon's server-side functionality is summarized entirely in its help message:

[//]: # (run "./tailon --help" to update the next section)

[//]: # (BEGIN HELP_USAGE)
```
Usage: tailon [options] <filespec> [<filespec> ...]

Tailon is a webapp for searching through files and streams.

  -a, --allow-download         Allow file downloads (default true)
  -b, --bind string            Address and port to listen on (default ":8080")
  -h, --help                   Show this help message and exit
  -r, --relative-root string   Webapp relative root (default "/")

Tailon is configured entirely through command-line flags.

The command-line interface expects one or more <filespec> arguments, which
specify the files to serve. The format is:

  [alias=name,group=name]<source>

The "source" specifier can be a file name, glob or directory. The optional
"alias=" and "group=" specifiers change the display name of files in the UI
and the group in which they appear.

A file specifier points to a single, possibly non-existent file. The file
name can be overwritten with "alias=". For example:

  tailon alias=error.log,/var/log/apache/error.log

A glob evaluates to the list of files that match a shell filename pattern.
The pattern is evaluated each time the file list is refreshed. An "alias="
specifier overwrites the parent directory of each matched file in the UI.

  tailon "/var/log/apache/*.log" "alias=nginx,/var/log/nginx/*.log"

If a directory is given, all files under it are served recursively.

  tailon /var/log/apache/ /var/log/nginx/

Example usage:
  tailon file1.txt file2.txt file3.txt
  tailon alias=messages,/var/log/messages "/var/log/*.log"
  tailon -b localhost:8080,localhost:8081 /var/log/messages
```
[//]: # (END HELP_USAGE)

## Security

Tailon does not run external commands or invoke a shell. The filter is a Go
([RE2]) regular expression applied in-process, so there is no risk of shell or
command injection from anything entered in the UI.

Tailon serves — and, unless `--allow-download=false` is set, lets clients
download — exactly the files you point it at on the command line. It performs no
authentication: by default it is reachable by anyone who can connect to its
address and port. Restrict the bind address or put it behind an authenticating
reverse proxy if that matters for your deployment.

## Development

### Frontend

The frontend is plain, framework-free HTML, CSS and JavaScript. The static
assets live in `frontend/dist` and the Go templates in `frontend/templates`;
both are embedded into the binary at compile time with `//go:embed` (see
`frontend.go`). There is no build step or toolchain — edit the files directly
and rebuild the binary. The UI talks to the backend over Server-Sent Events.

### Backend

The backend is written in straightforward Go using **only the standard library** —
there are no third-party dependencies. Flag parsing, configuration, HTTP serving,
access logging, file following and regexp filtering for the Server-Sent Events
stream are all standard library. File reading and following live in `tailer.go`.

### TODO

* Basic and digest authentication.

* Add a 'command' filespec - e.g. `"command,journalctl -u nginx"`.

* Better configuration dialog.

* Add interface themes - e.g. light, dark and solarized.

* Add ability to change font family and size.

* Windows support (can use one of the Go tail implementations).

* Implement [wtee].

* Support ANSI color codes.

### Testing

Run the unit tests with `go test ./...`.

## What about the other tailon project?

This project began as a full rewrite of the original [tailon-legacy] with the
following goals in mind:

* Reduce maintenance overhead (especially on the frontend).
* Remove unwanted features and fix poor design choices.

In terms of tech, the following changed from the legacy project:

* Backend from Python+Tornado to Go.
* Frontend from a custom TypeScript solution to framework-free vanilla
  JavaScript (this fork removed the intermediate Vue.js app).
* No asset pipeline — the static files are embedded directly into the binary.
* No configuration file and no external tools — file reading, following and
  regexp filtering are all native Go; settings come from command-line flags.
* Fully self-contained executable with no third-party dependencies.

## Similar Projects

* [clarity]
* [errorlog]
* [log.io]
* [rtail]
* [tailon-legacy]

Attributions
------------

Tailon's favicon was created from [this icon].

## License

Tailon is released under the terms of the [Apache 2.0 License].

[gvalkov/tailon]: https://github.com/gvalkov/tailon
[tailon-legacy]:  https://github.com/gvalkov/tailon-legacy
[syslog-ng]:      https://www.syslog-ng.com/
[clarity]:        https://github.com/tobi/clarity
[wtee]:           https://github.com/gvalkov/wtee
[releases]:       https://github.com/tbocek/tailon/releases
[errorlog]:       http://www.psychogenic.com/en/products/Errorlog.php
[log.io]:         http://logio.org/
[rtail]:          http://rtail.org/
[this icon]:      http://www.iconfinder.com/icondetails/15150/48/terminal_icon
[RE2]:            https://github.com/google/re2/wiki/Syntax
[Apache 2.0 License]: LICENSE
