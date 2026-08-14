package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// testEvent is the go test -json event format (cmd/test2json).
type testEvent struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
	Output  string
}

type testResult struct {
	Package  string  `json:"package"`
	Name     string  `json:"name"`
	Status   string  `json:"status"` // pass | fail | skip
	Elapsed  float64 `json:"elapsed"`
	Output   string  `json:"output"`
	FailFile string  `json:"fail_file,omitempty"`
	FailLine int     `json:"fail_line,omitempty"`
}

type pkgResult struct {
	Package string  `json:"package"`
	Status  string  `json:"status"` // pass | fail | skip | build-fail
	Elapsed float64 `json:"elapsed"`
	Pass    int     `json:"pass"`
	Fail    int     `json:"fail"`
	Skip    int     `json:"skip"`
	Output  string  `json:"output,omitempty"`
}

type testRunResult struct {
	Ok           bool          `json:"ok"`
	Packages     []*pkgResult  `json:"packages"`
	Tests        []*testResult `json:"tests"`
	Pass         int           `json:"pass"`
	Fail         int           `json:"fail"`
	Skip         int           `json:"skip"`
	BuildOutput  string        `json:"build_output,omitempty"`
	Cover        *coverResult  `json:"cover,omitempty"`
	CoverProfile string        `json:"cover_profile,omitempty"`
}

var failLocRe = regexp.MustCompile(`(?m)^\s+((?:[a-zA-Z]:)?[^\s:]+\.go):(\d+):`)

// noiseLine matches test2json framing we do not want in captured output.
var noiseLine = regexp.MustCompile(`^(=== (RUN|PAUSE|CONT|NAME)|--- (PASS|FAIL|SKIP)|PASS$|FAIL$|ok\s|FAIL\s)`)

// cmdTest runs `go test -json`, aggregates the event stream into per-test and
// per-package results, and optionally attaches coverage data.
func cmdTest(argv []string) error {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	dir := fs.String("dir", ".", "directory to run in")
	run := fs.String("run", "", "-run pattern")
	pkg := fs.String("pkg", "./...", "package pattern(s), space separated")
	cover := fs.Bool("cover", false, "collect coverage profile")
	race := fs.Bool("race", false, "enable -race")
	timeout := fs.String("timeout", "", "go test -timeout value")
	count := fs.Int("count", 0, "go test -count value (0 = default)")
	verbose := fs.Bool("verbose", false, "pass -v (capture output of passing tests)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	patterns := strings.Fields(*pkg)
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	pkgDirs := listPackageDirs(*dir, patterns)

	args := []string{"test", "-json"}
	if *run != "" {
		args = append(args, "-run", *run)
	}
	if *race {
		args = append(args, "-race")
	}
	if *timeout != "" {
		args = append(args, "-timeout", *timeout)
	}
	if *count > 0 {
		args = append(args, "-count", strconv.Itoa(*count))
	}
	if *verbose {
		args = append(args, "-v")
	}
	var profile string
	if *cover {
		tmp, err := os.CreateTemp("", "hx-go-cover-*.out")
		if err != nil {
			return err
		}
		tmp.Close()
		profile = tmp.Name()
		mode := "count"
		if *race {
			mode = "atomic"
		}
		args = append(args, "-coverprofile="+profile, "-covermode="+mode)
	}
	args = append(args, patterns...)

	stdout, stderr, runErr := runGo(*dir, args...)

	res := &testRunResult{}
	tests := map[string]*testResult{}
	pkgs := map[string]*pkgResult{}
	var buildOut strings.Builder

	pkgFor := func(name string) *pkgResult {
		p, ok := pkgs[name]
		if !ok {
			p = &pkgResult{Package: name}
			pkgs[name] = p
			res.Packages = append(res.Packages, p)
		}
		return p
	}

	for _, line := range strings.Split(stdout, "\n") {
		if line == "" {
			continue
		}
		var ev testEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			// Non-JSON output on stdout: build noise.
			buildOut.WriteString(line + "\n")
			continue
		}
		switch {
		case ev.Test != "":
			key := ev.Package + "\x00" + ev.Test
			t, ok := tests[key]
			if !ok {
				t = &testResult{Package: ev.Package, Name: ev.Test}
				tests[key] = t
				res.Tests = append(res.Tests, t)
			}
			switch ev.Action {
			case "output":
				trimmed := strings.TrimLeft(strings.TrimRight(ev.Output, "\n"), " ")
				if !noiseLine.MatchString(trimmed) {
					t.Output += ev.Output
				}
			case "pass", "fail", "skip":
				t.Status = ev.Action
				t.Elapsed = ev.Elapsed
			}
		case ev.Package == "":
			// Toolchain output not tied to a package (e.g. compile errors).
			if ev.Output != "" {
				buildOut.WriteString(ev.Output)
			}
		default:
			p := pkgFor(ev.Package)
			switch ev.Action {
			case "output", "build-output":
				p.Output += ev.Output
			case "pass", "fail", "skip":
				p.Status = ev.Action
				p.Elapsed = ev.Elapsed
			case "build-fail":
				p.Status = "build-fail"
			}
		}
	}

	// Post-process tests: counts, fail locations, prune noise from pkg output.
	for _, t := range res.Tests {
		if t.Status == "" {
			t.Status = "fail" // interrupted (panic/timeout) — treat as failure
		}
		p := pkgFor(t.Package)
		switch t.Status {
		case "pass":
			p.Pass++
			res.Pass++
			if !*verbose {
				t.Output = "" // only keep output for failures unless -v
			}
		case "fail":
			p.Fail++
			res.Fail++
		case "skip":
			p.Skip++
			res.Skip++
		}
		if m := failLocRe.FindStringSubmatch(t.Output); m != nil {
			file := m[1]
			lineNo, _ := strconv.Atoi(m[2])
			if !filepath.IsAbs(file) {
				if d, ok := pkgDirs[t.Package]; ok {
					file = filepath.Join(d, file)
				}
			}
			t.FailFile = file
			t.FailLine = lineNo
		}
		t.Output = strings.TrimRight(t.Output, "\n")
	}
	for _, p := range res.Packages {
		if p.Status == "fail" && p.Fail == 0 && p.Pass == 0 && p.Skip == 0 {
			p.Status = "build-fail"
		}
		if p.Status != "fail" && p.Status != "build-fail" {
			p.Output = "" // only keep package output when something went wrong
		} else {
			p.Output = strings.TrimRight(p.Output, "\n")
		}
	}
	if stderr != "" {
		buildOut.WriteString(stderr)
	}
	res.BuildOutput = strings.TrimSpace(buildOut.String())

	// Sort: failing packages first, then by name; failing tests first within.
	sort.SliceStable(res.Packages, func(i, j int) bool {
		pi, pj := res.Packages[i], res.Packages[j]
		if bad(pi.Status) != bad(pj.Status) {
			return bad(pi.Status)
		}
		return pi.Package < pj.Package
	})
	sort.SliceStable(res.Tests, func(i, j int) bool {
		ti, tj := res.Tests[i], res.Tests[j]
		if ti.Package != tj.Package {
			return ti.Package < tj.Package
		}
		if bad(ti.Status) != bad(tj.Status) {
			return bad(ti.Status)
		}
		return ti.Name < tj.Name
	})

	// Never emit null arrays — the Steel side iterates these directly.
	if res.Tests == nil {
		res.Tests = []*testResult{}
	}
	if res.Packages == nil {
		res.Packages = []*pkgResult{}
	}

	res.Ok = runErr == nil && res.Fail == 0
	// A run with zero tests and a build failure should not report ok.
	if res.BuildOutput != "" && len(res.Tests) == 0 && runErr != nil {
		res.Ok = false
	}

	if *cover && res.BuildOutput == "" {
		if cov, err := buildCoverResult(profile, *dir, pkgDirs); err == nil {
			res.Cover = cov
			res.CoverProfile = profile
		} else {
			res.BuildOutput = "coverage: " + err.Error()
		}
	}

	emit(res)
	return nil
}

func bad(status string) bool {
	return status == "fail" || status == "build-fail"
}

// listPackageDirs maps import paths to directories for the given patterns.
func listPackageDirs(dir string, patterns []string) map[string]string {
	args := append([]string{"list", "-e", "-f", "{{.ImportPath}}\t{{.Dir}}"}, patterns...)
	out, _, err := runGo(dir, args...)
	dirs := map[string]string{}
	if err != nil {
		return dirs
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			dirs[parts[0]] = parts[1]
		}
	}
	return dirs
}

// cmdCover re-emits coverage data from an existing profile (e.g. to re-apply
// hints without re-running the tests).
func cmdCover(argv []string) error {
	fs := flag.NewFlagSet("cover", flag.ContinueOnError)
	profile := fs.String("profile", "", "coverage profile path")
	dir := fs.String("dir", ".", "module/package directory for import path resolution")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *profile == "" {
		return fmt.Errorf("cover: -profile is required")
	}
	pkgDirs := listPackageDirs(*dir, []string{"./..."})
	cov, err := buildCoverResult(*profile, *dir, pkgDirs)
	if err != nil {
		return err
	}
	emit(map[string]any{"cover": cov, "cover_profile": *profile})
	return nil
}
