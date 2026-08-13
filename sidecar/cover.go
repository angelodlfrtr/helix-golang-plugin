package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

type coverBlock struct {
	StartLine int `json:"start_line"`
	StartCol  int `json:"start_col"`
	EndLine   int `json:"end_line"`
	EndCol    int `json:"end_col"`
	NumStmt   int `json:"num_stmt"`
	Count     int `json:"count"`
}

type fileCover struct {
	File         string       `json:"file"` // absolute path when resolvable
	Blocks       []coverBlock `json:"blocks"`
	CoveredStmts int          `json:"covered_stmts"`
	TotalStmts   int          `json:"total_stmts"`
	Pct          float64      `json:"pct"`
}

type funcCover struct {
	Name string  `json:"name"`
	File string  `json:"file"`
	Line int     `json:"line"`
	Pct  float64 `json:"pct"`
}

type coverResult struct {
	TotalPct float64      `json:"total_pct"`
	Files    []*fileCover `json:"files"`
	Funcs    []funcCover  `json:"funcs"`
}

// profileLineRe: "import/path/file.go:23.10,29.2 3 1"
var profileLineRe = regexp.MustCompile(`^(.+):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)$`)

// buildCoverResult parses a coverage profile and resolves the import-path
// qualified file names to absolute paths using pkgDirs (import path -> dir),
// falling back to the module path mapping.
func buildCoverResult(profile, dir string, pkgDirs map[string]string) (*coverResult, error) {
	f, err := os.Open(profile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	files := map[string]*fileCover{}
	var order []string

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || len(line) > 5 && line[:5] == "mode:" {
			continue
		}
		m := profileLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		fc, ok := files[name]
		if !ok {
			fc = &fileCover{File: name}
			files[name] = fc
			order = append(order, name)
		}
		b := coverBlock{
			StartLine: atoi(m[2]), StartCol: atoi(m[3]),
			EndLine: atoi(m[4]), EndCol: atoi(m[5]),
			NumStmt: atoi(m[6]), Count: atoi(m[7]),
		}
		fc.Blocks = append(fc.Blocks, b)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("empty coverage profile %s", profile)
	}

	res := &coverResult{}
	var covered, total int
	for _, name := range order {
		fc := files[name]
		fc.File = resolveProfilePath(name, dir, pkgDirs)
		for _, b := range fc.Blocks {
			fc.TotalStmts += b.NumStmt
			if b.Count > 0 {
				fc.CoveredStmts += b.NumStmt
			}
		}
		covered += fc.CoveredStmts
		total += fc.TotalStmts
		if fc.TotalStmts > 0 {
			fc.Pct = 100 * float64(fc.CoveredStmts) / float64(fc.TotalStmts)
		}
		res.Files = append(res.Files, fc)
		res.Funcs = append(res.Funcs, fileFuncCoverage(fc)...)
	}
	if total > 0 {
		res.TotalPct = 100 * float64(covered) / float64(total)
	}
	sort.Slice(res.Funcs, func(i, j int) bool { return res.Funcs[i].Pct < res.Funcs[j].Pct })
	return res, nil
}

// resolveProfilePath maps "module/pkg/file.go" to an absolute path.
func resolveProfilePath(name, dir string, pkgDirs map[string]string) string {
	if filepath.IsAbs(name) {
		return name
	}
	pkgPath := path.Dir(name)
	if d, ok := pkgDirs[pkgPath]; ok {
		return filepath.Join(d, path.Base(name))
	}
	// Fallback: main module mapping.
	out, _, err := runGo(dir, "list", "-m", "-f", "{{.Path}}\t{{.Dir}}")
	if err == nil {
		var modPath, modDir string
		fmt.Sscanf(out, "%s\t%s", &modPath, &modDir)
		if modPath != "" && modDir != "" && len(name) > len(modPath) && name[:len(modPath)] == modPath {
			return filepath.Join(modDir, filepath.FromSlash(name[len(modPath)+1:]))
		}
	}
	return name
}

// fileFuncCoverage computes per-function coverage the way
// `go tool cover -func` does: statements of blocks within the func's extent.
func fileFuncCoverage(fc *fileCover) []funcCover {
	if !filepath.IsAbs(fc.File) {
		return nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, fc.File, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var funcs []funcCover
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		start := fset.Position(fd.Pos())
		end := fset.Position(fd.End())
		var covered, total int
		for _, b := range fc.Blocks {
			if b.StartLine >= start.Line && b.EndLine <= end.Line {
				total += b.NumStmt
				if b.Count > 0 {
					covered += b.NumStmt
				}
			}
		}
		if total == 0 {
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			name = "(" + exprString(fset, fd.Recv.List[0].Type) + ")." + name
		}
		funcs = append(funcs, funcCover{
			Name: name,
			File: fc.File,
			Line: start.Line,
			Pct:  100 * float64(covered) / float64(total),
		})
	}
	return funcs
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
