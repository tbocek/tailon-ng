#!/usr/bin/env bash
# Generates docs/demo.html from the real frontend (frontend/*.html, main.css,
# main.js): one self-contained page with window.DEMO set, so there is no
# server at all — you drag a log file onto it and it renders entirely in the
# browser; nothing is ever uploaded. Re-run after frontend changes.
set -euo pipefail
cd "$(dirname "$0")"

python3 - <<'EOF'
import re, pathlib

base = pathlib.Path("frontend/base.html").read_text()
body = pathlib.Path("frontend/tailon.html").read_text()
css = pathlib.Path("frontend/main.css").read_text()
js = pathlib.Path("frontend/main.js").read_text()

m = re.search(r'{{\s*define "body"\s*}}(.*){{\s*end\s*}}\s*$', body, re.S)
b = m.group(1).replace("{{.Version}}", "demo")

html = base
html = re.sub(r'{{\s*template "title" \.\s*}}', "Tailon-ng — live demo", html)
html = re.sub(r'{{\s*template "description"\s*}}',
              "Drag a log file in — it renders in your browser and never leaves it", html)
html = html.replace('<link rel="stylesheet" href="{{.RelativeRoot}}vfs/main.css">',
                    "<style>\n" + css + "</style>")
html = html.replace('<script src="{{.RelativeRoot}}vfs/main.js" defer></script>', "")
html = html.replace("var relativeRoot = {{.RelativeRoot}};",
                    'var relativeRoot = "/"; window.DEMO = true;')
html = re.sub(r'{{\s*template "body" \.\s*}}',
              lambda _: b + "\n<script>\n" + js + "\n</script>", html)

assert "{{" not in html, "unresolved template tokens in demo.html"
pathlib.Path("docs/demo.html").write_text(html)
print("docs/demo.html: %d bytes" % len(html))
EOF
