package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type modInfo struct {
	Path     string `json:"path"`
	Version  string `json:"version"`
	Main     bool   `json:"main"`
	Indirect bool   `json:"indirect"`
	// NewVersion is set when `go list -m -u` reports an available update.
	NewVersion string `json:"new_version,omitempty"`
	Replaced   string `json:"replaced,omitempty"`
}

// goListModule mirrors the -json output of `go list -m`.
type goListModule struct {
	Path     string
	Version  string
	Main     bool
	Indirect bool
	Update   *struct {
		Path    string
		Version string
	}
	Replace *struct {
		Path    string
		Version string
		Dir     string
	}
}

// cmdMod lists module dependencies (with available updates), upgrades one,
// or tidies the module. Vendored modules are handled throughout: `all` cannot
// be computed from a vendor directory, so -mod=mod is used to bypass it, and
// anything that rewrites go.mod re-syncs vendor/ afterwards.
func cmdMod(argv []string) error {
	fs := flag.NewFlagSet("mod", flag.ContinueOnError)
	dir := fs.String("dir", ".", "directory inside the module")
	action := fs.String("action", "list", "list | upgrade | tidy")
	module := fs.String("module", "", "module path (for upgrade)")
	checkUpdates := fs.Bool("check-updates", true, "query the proxy for newer versions (list)")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	switch *action {
	case "list":
		return modList(*dir, *checkUpdates)
	case "upgrade":
		if *module == "" {
			return fmt.Errorf("mod: -module is required for upgrade")
		}
		return modWrite(*dir, []string{"get", *module + "@latest"})
	case "tidy":
		return modWrite(*dir, []string{"mod", "tidy"})
	default:
		return fmt.Errorf("mod: unknown action %q", *action)
	}
}

// modWrite runs a command that rewrites go.mod, re-vendoring when needed so
// the vendor directory does not go stale behind the user's back.
func modWrite(dir string, args []string) error {
	out, errOut, err := runGo(dir, args...)
	output := strings.TrimSpace(out + errOut)
	if err == nil && isVendored(dir) {
		vOut, vErr, vErrRun := runGo(dir, "mod", "vendor")
		if extra := strings.TrimSpace(vOut + vErr); extra != "" {
			output = strings.TrimSpace(output + "\n" + extra)
		}
		if vErrRun != nil {
			err = vErrRun
			if output == "" {
				output = "go mod vendor failed"
			}
		}
	}
	emit(map[string]any{"ok": err == nil, "output": output})
	return nil
}

// isVendored reports whether the module containing dir uses a vendor directory.
func isVendored(dir string) bool {
	root := findModuleRoot(dir)
	if root == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(root, "vendor", "modules.txt"))
	return err == nil
}

func modList(dir string, checkUpdates bool) error {
	vendored := isVendored(dir)

	// With a vendor directory, `go list -m all` refuses to compute the module
	// graph (and -u refuses to look for upgrades); -mod=mod bypasses both.
	base := []string{"list", "-m"}
	if vendored {
		base = append(base, "-mod=mod")
	}
	withArgs := func(extra ...string) []string {
		return append(append(append([]string{}, base...), extra...), "-json", "all")
	}

	var out, stderr string
	var err error
	updatesChecked := false
	note := ""

	if checkUpdates {
		out, stderr, err = runGo(dir, withArgs("-u")...)
		if err == nil {
			updatesChecked = true
		} else {
			note = firstLine(stderr)
		}
	}
	if !updatesChecked {
		// Offline, or the proxy is unreachable — list without update checks.
		out, stderr, err = runGo(dir, withArgs()...)
	}

	if err != nil {
		// Fully offline with an incomplete module cache: the vendor manifest
		// still records every dependency and its version.
		if vendored {
			if mods, vErr := parseVendorModules(dir); vErr == nil {
				emit(map[string]any{
					"modules":         mods,
					"updates_checked": false,
					"note":            "listed from vendor/modules.txt",
				})
				return nil
			}
		}
		return fmt.Errorf("go list -m: %v: %s", err, firstLine(stderr))
	}

	modules := []modInfo{}
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var m goListModule
		if err := dec.Decode(&m); err != nil {
			return fmt.Errorf("parsing go list output: %v", err)
		}
		info := modInfo{
			Path:     m.Path,
			Version:  m.Version,
			Main:     m.Main,
			Indirect: m.Indirect,
		}
		if m.Update != nil {
			info.NewVersion = m.Update.Version
		}
		if m.Replace != nil {
			target := m.Replace.Path
			if m.Replace.Version != "" {
				target += "@" + m.Replace.Version
			}
			info.Replaced = target
		}
		modules = append(modules, info)
	}
	emit(map[string]any{
		"modules":         modules,
		"updates_checked": updatesChecked,
		"note":            note,
	})
	return nil
}

// parseVendorModules reads vendor/modules.txt, which lists every vendored
// module and version and works with no network and no module cache.
func parseVendorModules(dir string) ([]modInfo, error) {
	root := findModuleRoot(dir)
	if root == "" {
		return nil, fmt.Errorf("no go.mod found")
	}
	f, err := os.Open(filepath.Join(root, "vendor", "modules.txt"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	mods := []modInfo{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "## "):
			// "## explicit; go 1.22" marks the preceding module as a direct
			// dependency of the main module.
			if len(mods) > 0 && strings.Contains(line, "explicit") {
				mods[len(mods)-1].Indirect = false
			}
		case strings.HasPrefix(line, "# "):
			// "# module/path v1.2.3" or "# module/path v1.2.3 => other v1.0.0"
			fields := strings.Fields(strings.TrimPrefix(line, "# "))
			if len(fields) < 2 {
				continue
			}
			// A bare "# path => replacement" line restates an earlier entry.
			if fields[1] == "=>" {
				continue
			}
			m := modInfo{Path: fields[0], Version: fields[1], Indirect: true}
			for i, f := range fields {
				if f == "=>" && i+1 < len(fields) {
					m.Replaced = strings.Join(fields[i+1:], "@")
					break
				}
			}
			mods = append(mods, m)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(mods) == 0 {
		return nil, fmt.Errorf("no modules in vendor/modules.txt")
	}
	return mods, nil
}

// firstLine keeps panel messages to something that fits on screen.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
