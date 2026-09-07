# Vendored frontend assets — versions and hashes

CI verifies these files with `sha256sum -c static/vendor/*.sha256`
from the repo root. Record the exact file, version, URL, and hash below
whenever a vendored file is added or upgraded.

## Pins (researched 2026-09-05)

| Asset | Version | Source |
|---|---|---|
| Go toolchain | 1.26.5 (`go 1.26.5` in go.mod) | local toolchain; floor is 1.26 because goose v3.28 requires Go >= 1.25 |
| htmx core (`htmx.min.js`) | 2.0.10 | `https://cdn.jsdelivr.net/npm/htmx.org@2.0.10/dist/htmx.min.js` — stay on 2.x; 4.0.0-beta changes SSE syntax |
| SSE extension (`sse.js`) | htmx-ext-sse 2.2.4 | separate package from core in htmx 2.x — the old bundled 1.x file silently breaks on htmx 2 |
| Idiomorph extension (`idiomorph-ext.min.js`) | 0.7.4 | morph swaps (`morph:outerHTML`); 0.7.4 fixes the morph-degradation bug |
| Alpine.js (`alpine.min.js`) | 3.15.12 | 3.15.x line |
| Caveat (`caveat-v23-latin.woff2`) | Caveat v23 (variable 600–700, latin) | `https://fonts.gstatic.com/s/caveat/v23/Wnz6HAc5bAfYB2Q7ZjYYiAzcPA.woff2` — display face (OFL); latin subset resolved via `fonts.googleapis.com/css2` with a browser UA |
| IBM Plex Sans (`ibmplexsans-v23-latin.woff2`) | IBM Plex Sans v23 (variable 400–600, latin) | `https://fonts.gstatic.com/s/ibmplexsans/v23/zYXzKVElMYYaJe8bpLHnCwDKr932-G7dytD-Dmu1syxeKYbSB4Zh.woff2` — UI face (OFL) |
| IBM Plex Mono 400 (`ibmplexmono-v20-400-latin.woff2`) | IBM Plex Mono v20, 400, latin | `https://fonts.gstatic.com/s/ibmplexmono/v20/-F63fjptAgt5VM-kVkqdyU8n1i8q131nj-o.woff2` — instrument face (OFL) |
| IBM Plex Mono 500 (`ibmplexmono-v20-500-latin.woff2`) | IBM Plex Mono v20, 500, latin | `https://fonts.gstatic.com/s/ibmplexmono/v20/-F6qfjptAgt5VM-kVkqdyU8n3twJwlBFgsAXHNk.woff2` — instrument face (OFL) |
| a-h/templ (Go, not vendored here) | v0.3.1020 (`tool` directive) | `go run github.com/a-h/templ/cmd/templ generate` reproduces codegen |
| modernc.org/sqlite (Go) | v1.58.0 | pure-Go, `CGO_ENABLED=0`; pin the matching `modernc.org/libc` it requires |
| pressly/goose/v3 (Go) | v3.28.0 | `SetBaseFS` + `SetDialect("sqlite3")` + `Up` |

## Files

| File | Version | Source URL | sha256 |
|---|---|---|---|
| `htmx.min.js` | htmx.org 2.0.10 | `https://cdn.jsdelivr.net/npm/htmx.org@2.0.10/dist/htmx.min.js` | `71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de` |
| `sse.js` | htmx-ext-sse 2.2.4 | `https://cdn.jsdelivr.net/npm/htmx-ext-sse@2.2.4/sse.js` | `3b5992a541619babefc4c169505af474df5c3039da51e59b96ccf9241ecd61d2` |
| `idiomorph-ext.min.js` | idiomorph 0.7.4 | `https://cdn.jsdelivr.net/npm/idiomorph@0.7.4/dist/idiomorph-ext.min.js` | `a6437e55b1b6a07bc421f0d230266a39399b6826c6ed19e0ed9c63b707444a5f` |
| `alpine.min.js` | alpinejs 3.15.12 | `https://cdn.jsdelivr.net/npm/alpinejs@3.15.12/dist/cdn.min.js` | `57b37d7cae9a27d965fdae4adcc844245dfdc407e655aee85dcfff3a08036a3f` |
| `fonts/caveat-v23-latin.woff2` | Caveat v23 (600–700) | `https://fonts.gstatic.com/s/caveat/v23/Wnz6HAc5bAfYB2Q7ZjYYiAzcPA.woff2` | `7a0049db01fdd8fc7af7ef36675862a72a5c76d0cb45ae63c9f5b6f1b769dac7` |
| `fonts/ibmplexsans-v23-latin.woff2` | IBM Plex Sans v23 (400–600) | `https://fonts.gstatic.com/s/ibmplexsans/v23/zYXzKVElMYYaJe8bpLHnCwDKr932-G7dytD-Dmu1syxeKYbSB4Zh.woff2` | `056e4e2459f57a0033c8c9c844ff19d6e42ac8602027803d4345823bcc939818` |
| `fonts/ibmplexmono-v20-400-latin.woff2` | IBM Plex Mono v20 (400) | `https://fonts.gstatic.com/s/ibmplexmono/v20/-F63fjptAgt5VM-kVkqdyU8n1i8q131nj-o.woff2` | `c36f509c0a8f9f85f29cb44bc8701d8a9e0b14c499e77a884f789ead7093a7ac` |
| `fonts/ibmplexmono-v20-500-latin.woff2` | IBM Plex Mono v20 (500) | `https://fonts.gstatic.com/s/ibmplexmono/v20/-F6qfjptAgt5VM-kVkqdyU8n3twJwlBFgsAXHNk.woff2` | `a76f53ca6612e7b3828eec2311098675b7f9849ae4169a8bcef6302aec02a6c0` |

Each file has a `<name>.sha256` sidecar; verify with
`sha256sum -c static/vendor/*.sha256` from the repo root. Only
third-party vendored bytes are hashed; first-party `app.css` and
`fonts.css` are not (they change with the app). PicoCSS was removed:
styling is bespoke in `app.css` ("no Pico").
