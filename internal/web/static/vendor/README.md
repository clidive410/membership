# Vendored front-end assets

Served from the binary (`go:embed`) under `/static/vendor/`. No CDN is
used at runtime, and there is no npm or JavaScript build step: the files
are copied unmodified from the published npm tarballs.

| Package | Version | Licence | Files | Source |
|---------|---------|---------|-------|--------|
| [@tabler/core](https://tabler.io) (Bootstrap 5 based) | 1.6.1 | MIT, see `tabler/LICENSE` | `tabler/css/tabler.min.css` | https://registry.npmjs.org/@tabler/core/-/core-1.6.1.tgz, `package/dist/css/tabler.min.css` |
| [@tabler/icons](https://tabler.io/icons) | 3.48.0 | MIT, see `tabler-icons/LICENSE` | 26 outline SVGs in `tabler-icons/outline/` | https://registry.npmjs.org/@tabler/icons/-/icons-3.48.0.tgz, `package/icons/outline/*.svg` |
| [@fontsource/open-sans-condensed](https://fontsource.org/fonts/open-sans-condensed) (the club site's heading font) | 4.5.5 (font v21) | Apache 2.0, see `open-sans-condensed/LICENSE` | `open-sans-condensed/open-sans-condensed-latin-700-normal.woff2` (Bold, latin subset only) | https://registry.npmjs.org/@fontsource/open-sans-condensed/-/open-sans-condensed-4.5.5.tgz, `package/files/` |

Tabler's JavaScript is not used. The few interactive bits (sidebar on small
screens, tick/untick all) work with plain links and CSS. The condensed font is
used only for the wordmark, page titles and sidebar labels (`@font-face` in
`../style.css`); body text stays on Tabler's system font stack. The icons are
inlined into the HTML by the `icon` template function
(`internal/web/render.go`), which reads them from `tabler-icons/outline/`;
add an icon by copying its SVG from the same package version.

Checksums (SHA-256):

- `core-1.6.1.tgz`: `d02d679e51f6c179b21f035e154c13cdcb2ae8b18579e56ea606408ba7fc7a87`
  (npm integrity `sha512-lgmR9gcxRp5JlLxcmRi9Rz85JrEvH6Bz71KnlcPVSa97wtwQNjgSs3g3fxwbAlulHyRMghhIV04QUuW3pqDsIg==`)
- `tabler/css/tabler.min.css`: `370a2d4e608ae518351d10528fc7dbe19b354ae5062c51b2a9cb294850d65403`
- `open-sans-condensed-4.5.5.tgz`: `0b6997197323101e76e754e91ad8315eeb0fe384ab73f1c7d304afcea78ca1aa`
  (npm integrity `sha512-ah6DBSWgjWEG20tVJnvcUQ/8XTfnGpuOGK4HLTF91vtrGg7PyrdEMlao/O1+S4i+SzeuJCmf5Qbc3eWponBL3Q==`)
- `open-sans-condensed/open-sans-condensed-latin-700-normal.woff2`: `68116287d6b99feff98ad41fa01cdc251f12b52e253bab507ed2eaa7a363e2b5`
- `icons-3.48.0.tgz`: `28447dcf6f0bb2b8d92c59a1b3d30900a180de2e97d7db4d267e6320dc68f449`

To upgrade: download the new tarballs, replace the files, update this table
and the checksums, then check every page (the app's own overrides live in
`../style.css`).
