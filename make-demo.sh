#!/usr/bin/env bash
# Generates docs/demo.html from the real frontend (frontend/main.html,
# main.css, main.js): one self-contained page with window.DEMO set, so there
# is no server at all — you drag a log file onto it and it renders entirely in
# the browser; nothing is ever uploaded. Re-run after frontend changes.
set -euo pipefail
cd "$(dirname "$0")"

python3 - <<'EOF'
import re, pathlib

html = pathlib.Path("frontend/main.html").read_text()
css = pathlib.Path("frontend/main.css").read_text()
js = pathlib.Path("frontend/main.js").read_text()

html = html.replace("<title>Tailon-ng</title>", "<title>Tailon-ng — live demo</title>")
html = html.replace('content="Tail and search your log files from the browser"',
                    'content="Drag a log file in — it renders in your browser and never leaves it"')
html = html.replace('<link rel="stylesheet" href="{{.RelativeRoot}}main.css">',
                    "<style>\n" + css + "</style>")
html = html.replace('<script src="{{.RelativeRoot}}main.js" defer></script>', "")
html = html.replace("var relativeRoot = {{.RelativeRoot}};",
                    'var relativeRoot = "/"; window.DEMO = true;')
html = html.replace("{{.Version}}", "demo")
html = html.replace("    </body>", "<script>\n" + js + "\n</script>\n    </body>")

assert "{{" not in html, "unresolved template tokens in demo.html"
assert "window.DEMO" in html and "<style>" in html and "</script>" in html
pathlib.Path("docs/demo.html").write_text(html)
print("docs/demo.html: %d bytes" % len(html))

# Keep the website's lines-of-code claim honest: count the real thing and
# patch every "about N lines of code" occurrence (rounded to the nearest 100).
go_files = ["main.go", "server.go", "tailer.go", "filelister.go", "frontend.go",
            "watcher_linux.go", "watcher_other.go"]
def code_lines(path, comment):
    n = 0
    for line in pathlib.Path(path).read_text().splitlines():
        s = line.strip()
        if s and not s.startswith(comment):
            n += 1
    return n

backend = sum(code_lines(f, "//") for f in go_files)
frontend = code_lines("frontend/main.js", "//")
frontend += sum(1 for l in pathlib.Path("frontend/main.css").read_text().splitlines()
                if l.strip() and not l.strip().startswith(("/*", "*")))
frontend += sum(1 for l in pathlib.Path("frontend/main.html").read_text().splitlines()
                if l.strip())
# Each part is rounded to the nearest 100 and the total is the sum of the
# rounded parts, so the three numbers shown always add up.
fmt = lambda n: "{:,}".format(round(n / 100) * 100)
rounded = fmt(round(backend / 100) * 100 + round(frontend / 100) * 100)
split = "%s in the Go backend, %s in the browser frontend" % (fmt(backend), fmt(frontend))

idx = pathlib.Path("docs/index.html")
html = idx.read_text()
html, n = re.subn(r"([Aa]bout) [\d,]+ lines of code",
                  lambda m: "%s %s lines of code" % (m.group(1), rounded), html)
html, n2 = re.subn(r"[\d,]+ in the Go backend, [\d,]+ in the browser frontend", split, html)
idx.write_text(html)
print("lines of code: %d go + %d frontend -> 'about %s' (%s) — %d+%d spots patched"
      % (backend, frontend, rounded, split, n, n2))
EOF
