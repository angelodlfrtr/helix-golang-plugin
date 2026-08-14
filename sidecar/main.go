// hx-go-tool is the Go sidecar for the helix-golang-plugin Steel cog.
//
// Every subcommand prints a single JSON object on stdout and exits 0.
// Failures are reported as {"error": "..."} so the Steel side only ever
// has to parse stdout.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: hx-go-tool <enclosing|test|cover|tags|impl|gotests|mod> [flags]")
	}
	var err error
	switch os.Args[1] {
	case "enclosing":
		err = cmdEnclosing(os.Args[2:])
	case "test":
		err = cmdTest(os.Args[2:])
	case "cover":
		err = cmdCover(os.Args[2:])
	case "tags":
		err = cmdTags(os.Args[2:])
	case "impl":
		err = cmdImpl(os.Args[2:])
	case "gotests":
		err = cmdGotests(os.Args[2:])
	case "mod":
		err = cmdMod(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fail(err.Error())
	}
}

func emit(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stdout, `{"error":%q}`, "encode: "+err.Error())
	}
}

func fail(msg string) {
	emit(map[string]string{"error": msg})
	os.Exit(0)
}

// runGo runs the go tool in dir with a 10-minute timeout and returns stdout / stderr.
func runGo(dir string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// writeFileAtomic writes data to a temporary file in target's directory, then renames it over target.
func writeFileAtomic(target string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".hx-go-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName) // no-op if successfully renamed
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpName, target)
}

// findModuleRoot walks up from dir looking for go.mod.
func findModuleRoot(dir string) string {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
