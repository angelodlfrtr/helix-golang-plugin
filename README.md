# Helix editor Go plugin

Go development tools for the [Helix](https://helix-editor.com) editor, built with
[Steel](https://github.com/mattwparas/steel) (the Scheme scripting layer embedded in Helix)
and a Go sidecar binary that does the heavy lifting with `go/ast` and `go/types` —
no `gomodifytags`, `impl`, or `gotests` installs required.

> This project was entirely developed with Claude Code.

## Features

- **Test runner** — run the test under the cursor, the package, or the whole module
  (`go test -json` under the hood). Results land in a bottom panel: failures first and
  auto-expanded, jump to the failing line with `Enter`, re-run with `R`.
- **Coverage** — run package tests with a coverage profile and mark uncovered lines
  in the buffer (`● not covered` inlay hints), plus total / per-file percentages.
- **Struct tags** — add, remove, or clear field tags on the struct under the cursor,
  with snake/camel/lisp/pascal casing and options like `omitempty`. Initialism-aware
  (`AvatarURL` → `avatar_url`).
- **Interface stubs** — insert method stubs implementing any interface (stdlib,
  current module, or dependencies) at the cursor, `impl`-style.
- **Test skeletons** — generate a table-driven test for the function under the cursor
  into the sibling `_test.go` (methods, variadics, and error returns handled).
- **go.mod panel** — list dependencies with available updates; upgrade one (`u`),
  upgrade all (`U`), or `go mod tidy` (`t`) without leaving the editor.

Everything runs asynchronously on a background thread — the editor never blocks on
`go test`.

## Installation

1. Copy (or clone) this directory into your Steel cogs path, usually
   `~/.steel/cogs/helix-golang-plugin/`.
2. Build the sidecar (requires a Go toolchain):

   ```sh
   ~/.steel/cogs/helix-golang-plugin/build.sh
   ```

3. Require the plugin from your Helix Steel init file:

   ```scheme
   (require "helix-golang-plugin/go-tools.scm")
   ```

   and bind some keys — see [Example keymaps](#example-keymaps) below.

The sidecar binary is looked up at `<cog>/bin/hx-go-tool` first, then on `PATH`;
`(go-tools-set-sidecar! "/path/to/hx-go-tool")` overrides both.

## Example keymaps

All examples go in your Helix Steel `init.scm`, after the `require` above, and
also need `(require "helix/keymaps.scm")`.

**A global `Space o` "Go menu"** (recommended — one mnemonic prefix, "gO";
`Space g`/`Space G` are taken by Helix's debug menu):

```scheme
(keymap (global)
        (normal (space (o (t ":go-test")           ; test under cursor / package
                          (p ":go-test-package")
                          (a ":go-test-all")
                          (r ":go-test-rerun")
                          (o ":go-test-panel")     ; open/focus results panel
                          (c ":go-coverage")
                          (C ":go-coverage-clear")
                          (s ":go-tags-add")       ; prompts for tags
                          (S ":go-tags-remove")
                          (i ":go-impl")           ; prompts for recv + interface
                          (g ":go-gotests")
                          (m ":go-mod-panel")))))
```

`Space o t` runs the test under the cursor, `Space o o` focuses the results
panel, `Space o m` opens the dependency panel, and so on. If `Space o` is taken
in your config too, any free key works — the menu is self-contained.

**Bindings only in Go buffers** — extension-scoped maps allow shorter chords
that only exist in `.go` files:

```scheme
(keymap (extension "go")
        (normal (C-t ":go-test")            ; Ctrl-t: test under cursor
                (C-y ":go-test-panel")))    ; Ctrl-y: results panel
```

**Minimal, tests only:**

```scheme
(keymap (global)
        (normal (space (t ":go-test")
                       (T ":go-test-panel"))))
```

(Check that `Space t` doesn't collide with your own config first.)

Notes:

- Commands that take arguments can be bound with the arguments inline, the same
  as Helix's native config format, e.g. `(s ":go-tags-add json omitempty")` to
  skip the prompt.
- The panels bring their own keys (`j`/`k`, `Enter` to jump, `R` to re-run,
  `u`/`U`/`t` in the go.mod panel, `?` for a help overlay) — no bindings needed
  inside them.
- Restart Helix (or `:config-reload` if your build reloads Steel) to pick up
  changes.

## Commands

| Command | Action |
|---------|--------|
| `:go-test` | Run the test under the cursor; elsewhere, the current package's tests |
| `:go-test-package` | Run the current file's package tests |
| `:go-test-all` | Run every test in the module (`./...` from the module root) |
| `:go-test-rerun` | Repeat the last test run |
| `:go-test-panel` | Focus / toggle the test results panel |
| `:go-coverage` | Package tests with coverage; mark uncovered lines in this buffer |
| `:go-coverage-clear` | Remove the coverage marks |
| `:go-tags-add json,yaml [omitempty]` | Add tags to the struct under the cursor (prompts without args) |
| `:go-tags-remove json` | Remove tag keys from the struct under the cursor |
| `:go-tags-clear` | Remove all tags from the struct under the cursor |
| `:go-impl s *Server io.Reader` | Insert interface method stubs at the cursor (prompts without args) |
| `:go-gotests` | Generate a table-driven test for the function under the cursor |
| `:go-mod-panel` | Open the go.mod dependency panel |

### Test panel keys

| Key | Action |
|-----|--------|
| `j` / `k` / `↑` / `↓` | Navigate |
| `gg` / `G` | Top / bottom |
| `Ctrl-d` / `Ctrl-u` | Half page down / up |
| `o` / `Enter` | Jump to failure location / toggle output |
| `Tab` | Expand or collapse output |
| `R` | Re-run the last test run |
| `Esc` | Unfocus (panel stays open) |
| `q` | Close the panel |
| `?` | Help overlay |

### go.mod panel keys

`j`/`k` navigate, `u` upgrade selected to `@latest`, `U` upgrade everything with an
update, `t` runs `go mod tidy`, `R` re-checks updates, `q`/`Esc` closes, `?` help.

## Configuration

Optional — call these from your `init.scm` after the `require`:

```scheme
(go-tools-set-panel-height! 16)          ; test panel content rows (default 12)
(go-tools-set-race! #t)                  ; run tests with -race
(go-tools-set-verbose! #t)               ; keep output of passing tests too
(go-tools-set-timeout! "60s")            ; go test -timeout
(go-tools-set-tags-transform! "camelcase") ; tag casing for :go-tags-add
(go-tools-set-sidecar! "/opt/bin/hx-go-tool")
```

## How it works

The Steel side is UI only. All Go-aware work happens in `sidecar/` (a small
dependency-free Go program) which prints one JSON object per invocation:

- `test` wraps `go test -json`, aggregates the event stream, extracts failure
  `file:line` locations, and parses the coverage profile (per-file blocks and
  per-function percentages) when asked.
- `enclosing` finds the function under the cursor so `:go-test` knows what to run.
- `tags` rewrites only the declaration containing the struct, preserving the rest
  of the file byte-for-byte, then gofmts the result.
- `impl` resolves interfaces through `go list -export` + the stdlib `gc` importer,
  so it works offline for anything already in the build cache.
- `gotests` builds skeletons from the function's AST.
- `mod` wraps `go list -m -u -json all`, `go get`, and `go mod tidy`.

## Notes and caveats

- `:go-tags-add/-remove/-clear` and `:go-gotests` **save the buffer** first
  (they rewrite the file on disk, then reload it).
- `:go-impl` inserts code at the cursor; run your imports organizer afterwards if
  the stubs reference packages the file doesn't import yet.
- Coverage marks use Helix's inlay-hint API (experimental in helix-steel); clear
  them from the same buffer they were applied to.
- Update checks in the go.mod panel need network access; offline, the panel still
  lists dependencies and says "updates unknown".

## Requirements

- Helix with Steel scripting enabled
- A Go toolchain (1.22+) on `PATH`
