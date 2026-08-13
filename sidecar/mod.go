package main

import (
	"encoding/json"
	"flag"
	"fmt"
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
// or tidies the module.
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
		out, errOut, err := runGo(*dir, "get", *module+"@latest")
		emit(map[string]any{
			"ok":     err == nil,
			"output": strings.TrimSpace(out + errOut),
		})
		return nil
	case "tidy":
		out, errOut, err := runGo(*dir, "mod", "tidy")
		emit(map[string]any{
			"ok":     err == nil,
			"output": strings.TrimSpace(out + errOut),
		})
		return nil
	default:
		return fmt.Errorf("mod: unknown action %q", *action)
	}
}

func modList(dir string, checkUpdates bool) error {
	args := []string{"list", "-m", "-json"}
	if checkUpdates {
		args = []string{"list", "-m", "-u", "-json"}
	}
	args = append(args, "all")

	out, stderr, err := runGo(dir, args...)
	updatesChecked := checkUpdates
	note := ""
	if err != nil && checkUpdates {
		// Likely offline / proxy unreachable — retry without update checks.
		note = strings.TrimSpace(stderr)
		out, stderr, err = runGo(dir, "list", "-m", "-json", "all")
		updatesChecked = false
	}
	if err != nil {
		return fmt.Errorf("go list -m: %v: %s", err, strings.TrimSpace(stderr))
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
