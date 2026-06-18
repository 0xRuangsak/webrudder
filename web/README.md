# webrudder — web

Landing page for webrudder. Static marketing site (Bootstrap via CDN — no build step), deployed separately — **not** part of the running daemon. Content mirrors the [root README](../README.md).

**View:** open `index.html` in a browser, or serve the folder with any static server (`python3 -m http.server`).

**Deploy:** `.github/workflows/pages.yml` publishes this folder to GitHub Pages on push to `main`. Custom domain is set via `CNAME` (webrudder.xyz). One-time: repo Settings → Pages → Source = "GitHub Actions".

**Status:** first Bootstrap pass.
