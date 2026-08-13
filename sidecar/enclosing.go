package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

type enclosingResult struct {
	PackageName string `json:"package_name"`
	Dir         string `json:"dir"`
	ModuleDir   string `json:"module_dir"`
	FuncName    string `json:"func_name"`
	RecvType    string `json:"recv_type"`
	IsTestFile  bool   `json:"is_test_file"`
	// TestName is set when the cursor is inside a Test/Benchmark/Fuzz/Example
	// function in a _test.go file.
	TestName string `json:"test_name"`
}

// cmdEnclosing reports the function enclosing a cursor position, plus the
// package / module context — the Steel side uses it to decide what to run.
func cmdEnclosing(argv []string) error {
	fs := flag.NewFlagSet("enclosing", flag.ContinueOnError)
	file := fs.String("file", "", "source file")
	line := fs.Int("line", 0, "1-based cursor line")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *file == "" || *line <= 0 {
		return fmt.Errorf("enclosing: -file and -line are required")
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, *file, nil, parser.SkipObjectResolution)
	if err != nil {
		return err
	}

	dir := filepath.Dir(*file)
	res := enclosingResult{
		PackageName: f.Name.Name,
		Dir:         dir,
		ModuleDir:   findModuleRoot(dir),
		IsTestFile:  strings.HasSuffix(*file, "_test.go"),
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		start := fset.Position(fd.Pos()).Line
		end := fset.Position(fd.End()).Line
		if *line >= start && *line <= end {
			res.FuncName = fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) > 0 {
				res.RecvType = exprString(fset, fd.Recv.List[0].Type)
			}
			if res.IsTestFile && fd.Recv == nil && isTestFuncName(fd.Name.Name) {
				res.TestName = fd.Name.Name
			}
			break
		}
	}
	emit(res)
	return nil
}

// isTestFuncName mirrors the testing package's rule: the prefix must be
// followed by a non-lowercase rune (or nothing).
func isTestFuncName(name string) bool {
	for _, p := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if name == p {
			return true
		}
		if strings.HasPrefix(name, p) {
			r, _ := utf8.DecodeRuneInString(name[len(p):])
			if !unicode.IsLower(r) {
				return true
			}
		}
	}
	return false
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	var buf bytes.Buffer
	printer.Fprint(&buf, fset, e)
	return buf.String()
}
