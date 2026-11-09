# VISUAL.md — the look of our apps

<!-- open/gcn/ui 0.1.5 — copied by sync.sh; change it in open/gcn/ui, not in the copy. -->

Every web app we build looks like one family. The reference is **numeros**
(`open/bolha/numeros`); atualiza and id follow it too. The stylesheet and this document live in
**`open/gcn/ui`**: `sync.sh` copies them into each app (`assets/style.css`, `VISUAL.md`),
`check.sh` checks an app against the rules below, and `examples/layout.html` is the markup to
start from. `open/gcn/go-app-template` carries a synced copy, so a new app starts in the look.

Two families, same tokens and components, different accent:

| family | where | accent (light / dark) | "part of" |
|---|---|---|---|
| **bolha** | `open/bolha/*`, the community's tools | yellow `#F5B301` / `#FFC72C` | bolha.io |
| **gcn** | `open/gcn/*`, gutocarvalho.net tools | red `#D7263D` / `#FF4D5E` | gutocarvalho.net |

`<html data-family="gcn">` switches the accent; in the Go template, `new.sh` sets it from the
project's path (`GOAPP_FAMILY`).

## Rules (must)

1. **PT and EN, the whole app.** Every text the person sees exists in both languages (a test
   fails when a key is missing in one). Language comes from `?lang=`, then a cookie, then the
   browser's `Accept-Language`, then the configured default; the header has `PT · EN`.
2. **The stylesheet is the synced one.** `assets/style.css` comes from open/gcn/ui and is never
   edited in the app; app rules go in a second file (`assets/<app>.css`) that uses the tokens.
   **The tokens:** `--paper --surface --surface-2 --border --border-strong
   --text --text-dim --text-faint --accent --accent-strong --accent-soft --on-accent --ok --warn
   --danger --info --shadow --mono --sans`, with the values in `style.css`. No hex values
   in components: status colours use `--ok/--warn/--danger/--info`.
3. **Light and dark from the system** (`prefers-color-scheme`). Never a forced dark page
   (`:root:root{…}`, `color-scheme: dark` alone). The only exception is generated content (a
   PNG, a card to export), never the page around it.
4. **System fonts only.** `--sans: system-ui…`, `--mono: ui-monospace…`. No webfonts, no
   `assets/fonts/`, no `font-src` in the CSP. Exception: an image the app exports (poet's card)
   may carry its own fonts, scoped to that image (`check.sh --card-fonts`); the page around it
   uses the system fonts.
5. **Never vendor `artefatos.css`.** It is the stylesheet of the artifact pages, not of apps.
6. **Header**: `.top` with `.brand` (the `.bubble` in the accent + the app's own name — never
   "bolhaverse" or "app · domain"), and on the right the links as `.toggle` pills (release
   notes, account, sign out) and `PT · EN`.
7. **Hero**: `.kicker` (mono, uppercase, with the dot: "bolha.io · <what>"), an `h1` with the key
   word in `<em>` (accent-strong), a `.lede`; `.stamp` for state ("last round …", "signed in as …").
8. **Sections**: `.cat` (mono title + rule) above `.card`s (16px radius, border, shadow);
   numbers as `.tiles`, the important one `.tile.hl`.
9. **Pills for everything you click or that marks something**: primary button in the accent
   (`--on-accent` text), secondary with a border; tabs, filters, language and labels are pills
   (999px), not underlined links. Inputs and cards: 10–16px radius.
10. **Footer, fixed format**: `<app> X.Y.Z-BUILD · source code · part of bolha.io` (or
    gutocarvalho.net). Extras (release notes, upstream credit) go after. The version links to
    the release notes, not to the registry.
11. **Labels are explained.** When a page uses badges (major, stateful, new…), the page says what
    each one means (a legend under the table), in both languages.
12. **CSS in `/assets/<file>.css?v={{.Version}}`, no inline `style=`** (the CSP has
    `style-src 'self'`). JavaScript only when the page needs it, vanilla and progressive (the
    page works without it); a vendored library only when there is no other way. CSP starts from
    `default-src 'none'`.
13. **Accessible**: `aria-current` on the active link/language, labels on form fields, contrast
    of `--text-dim` on `--surface` as in the tokens (don't go lighter), focus visible.

## Class names the stylesheet owns

An app's own CSS must not reuse these names for something else (ajuda's category cards were
`.cat`, the section title, and grew its 40px margin): `.accent`, `.brand`, `.btn`, `.bubble`, `.card`, `.cat`, `.danger`, `.hero`, `.hl`, `.info`, `.kicker`, `.lab`, `.langsel`, `.lede`, `.legend`, `.mono`, `.ok`, `.pill`, `.prose`, `.release`, `.relnotes`, `.rule`, `.secondary`, `.small`, `.stamp`, `.tile`, `.tiles`, `.toggle`, `.top`, `.toplinks`, `.val`, `.warn`, `.when`, `.wrap`.
Overriding one of them on purpose (numeros's own sizes) is fine; reusing the name for another
thing is not.

## Checking an app

```sh
~/…/open/gcn/ui/check.sh ~/…/open/bolha/<app>             # FAIL/WARN lines; exit 1 on a FAIL
~/…/open/gcn/ui/check.sh --assets static ~/…/open/bolha/ajuda
```

It checks what a script can: no `artefatos.css`, webfonts or `font-src`; no `:root:root`; no
inline `style=`; the synced stylesheet, unedited and current; the skeleton (`data-family`, the
bubble, the kicker, the footer link, the PT/EN switch). The rest is the checklist.

## Checklist for a review

- [ ] PT and EN everywhere, switch in the header, both languages tested
- [ ] `check.sh` passes; light and dark both look right
- [ ] no webfonts, no `artefatos.css`, no inline styles, no hex in components
- [ ] header `.brand` + `.bubble` + app name; hero with kicker, `h1 em`, lede
- [ ] cards, pills, primary button in the accent; tabs and filters as pills
- [ ] footer `<app> version · source code · part of …`
- [ ] badges explained on the page
- [ ] screenshot of the main page, light and dark, in the merge request
