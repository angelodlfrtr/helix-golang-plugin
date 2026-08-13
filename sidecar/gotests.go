package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"unicode"
)

// cmdGotests generates a table-driven test skeleton for the function under
// the cursor, creating or appending to the sibling _test.go file.
func cmdGotests(argv []string) error {
	fs := flag.NewFlagSet("gotests", flag.ContinueOnError)
	file := fs.String("file", "", "source file")
	line := fs.Int("line", 0, "1-based cursor line")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *file == "" || *line <= 0 {
		return fmt.Errorf("gotests: -file and -line are required")
	}
	if strings.HasSuffix(*file, "_test.go") {
		return fmt.Errorf("gotests: cursor is already in a test file")
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, *file, nil, parser.SkipObjectResolution)
	if err != nil {
		return err
	}

	fd := funcAtOrAfterLine(fset, f, *line)
	if fd == nil {
		return fmt.Errorf("gotests: no function found at or after line %d", *line)
	}
	if fd.Type.TypeParams != nil && len(fd.Type.TypeParams.List) > 0 {
		return fmt.Errorf("gotests: generic functions are not supported yet")
	}

	testName := testFuncName(fd)
	testFile := strings.TrimSuffix(*file, ".go") + "_test.go"

	// Bail out (with the location) if the test already exists.
	if existing, err := os.ReadFile(testFile); err == nil {
		tf, err := parser.ParseFile(fset, testFile, existing, parser.SkipObjectResolution)
		if err == nil {
			for _, d := range tf.Decls {
				if tfd, ok := d.(*ast.FuncDecl); ok && tfd.Name.Name == testName {
					emit(map[string]any{
						"test_file": testFile,
						"test_name": testName,
						"line":      fset.Position(tfd.Pos()).Line,
						"already":   true,
					})
					return nil
				}
			}
		}
	}

	skeleton := buildTestSkeleton(fset, fd, testName)

	var content []byte
	insertLine := 0
	if existing, err := os.ReadFile(testFile); err == nil {
		trimmed := strings.TrimRight(string(existing), "\n")
		insertLine = strings.Count(trimmed, "\n") + 3 // blank line, then func
		content = []byte(trimmed + "\n\n" + skeleton)
	} else {
		header := "package " + f.Name.Name + "\n\nimport \"testing\"\n\n"
		insertLine = strings.Count(header, "\n") + 1
		content = []byte(header + skeleton)
	}
	formatted, err := format.Source(content)
	if err != nil {
		formatted = content // still write it; user can fix by hand
	}
	if err := os.WriteFile(testFile, formatted, 0o644); err != nil {
		return err
	}
	emit(map[string]any{"test_file": testFile, "test_name": testName, "line": insertLine, "already": false})
	return nil
}

func funcAtOrAfterLine(fset *token.FileSet, f *ast.File, line int) *ast.FuncDecl {
	var after *ast.FuncDecl
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		start := fset.Position(fd.Pos()).Line
		end := fset.Position(fd.End()).Line
		if line >= start && line <= end {
			return fd
		}
		if start > line && after == nil {
			after = fd
		}
	}
	return after
}

func testFuncName(fd *ast.FuncDecl) string {
	name := fd.Name.Name
	prefix := "Test"
	if !unicode.IsUpper([]rune(name)[0]) {
		prefix = "Test_"
	} else {
		name = string(unicode.ToUpper([]rune(name)[0])) + name[1:]
	}
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		base := fd.Recv.List[0].Type
		if se, ok := base.(*ast.StarExpr); ok {
			base = se.X
		}
		if id, ok := base.(*ast.Ident); ok {
			return "Test" + id.Name + "_" + fd.Name.Name
		}
	}
	return prefix + name
}

type testParam struct {
	name     string
	typ      string
	variadic bool
}

// buildTestSkeleton renders a gotests-style table-driven skeleton. Assertions
// are left as TODOs so the generated code never needs extra imports.
func buildTestSkeleton(fset *token.FileSet, fd *ast.FuncDecl, testName string) string {
	params := flattenParams(fset, fd.Type.Params)

	type want struct{ name, typ string }
	var wants []want
	hasErr := false
	if fd.Type.Results != nil {
		idx := 0
		results := flattenParams(fset, fd.Type.Results)
		for i, r := range results {
			if r.typ == "error" && i == len(results)-1 {
				hasErr = true
				break
			}
			n := "want"
			if idx > 0 {
				n = fmt.Sprintf("want%d", idx)
			}
			wants = append(wants, want{n, r.typ})
			idx++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "func %s(t *testing.T) {\n", testName)

	if len(params) > 0 {
		b.WriteString("\ttype args struct {\n")
		for _, p := range params {
			typ := p.typ
			if p.variadic {
				typ = "[]" + typ
			}
			fmt.Fprintf(&b, "\t\t%s %s\n", p.name, typ)
		}
		b.WriteString("\t}\n")
	}

	b.WriteString("\ttests := []struct {\n\t\tname string\n")
	if len(params) > 0 {
		b.WriteString("\t\targs args\n")
	}
	for _, w := range wants {
		fmt.Fprintf(&b, "\t\t%s %s\n", w.name, w.typ)
	}
	if hasErr {
		b.WriteString("\t\twantErr bool\n")
	}
	b.WriteString("\t}{\n\t\t// TODO: Add test cases.\n\t}\n")

	b.WriteString("\tfor _, tt := range tests {\n\t\tt.Run(tt.name, func(t *testing.T) {\n")

	callTarget := fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		recvType := exprString(fset, fd.Recv.List[0].Type)
		fmt.Fprintf(&b, "\t\t\tvar receiver %s // TODO: construct the receiver\n", recvType)
		callTarget = "receiver." + fd.Name.Name
	}

	var argParts []string
	for _, p := range params {
		a := "tt.args." + p.name
		if p.variadic {
			a += "..."
		}
		argParts = append(argParts, a)
	}
	call := fmt.Sprintf("%s(%s)", callTarget, strings.Join(argParts, ", "))

	var lhs []string
	for _, w := range wants {
		lhs = append(lhs, strings.Replace(w.name, "want", "got", 1))
	}
	if hasErr {
		lhs = append(lhs, "err")
	}

	switch {
	case len(lhs) == 0:
		fmt.Fprintf(&b, "\t\t\t%s\n", call)
	default:
		fmt.Fprintf(&b, "\t\t\t%s := %s\n", strings.Join(lhs, ", "), call)
	}
	if hasErr {
		fmt.Fprintf(&b, "\t\t\tif (err != nil) != tt.wantErr {\n\t\t\t\tt.Errorf(\"%s() error = %%v, wantErr %%v\", err, tt.wantErr)\n\t\t\t\treturn\n\t\t\t}\n", fd.Name.Name)
	}
	for _, w := range wants {
		got := strings.Replace(w.name, "want", "got", 1)
		fmt.Fprintf(&b, "\t\t\t_ = %s // TODO: assert %s against tt.%s\n", got, got, w.name)
	}
	b.WriteString("\t\t})\n\t}\n}\n")
	return b.String()
}

// flattenParams expands grouped fields ("a, b int") and names unnamed ones.
func flattenParams(fset *token.FileSet, fl *ast.FieldList) []testParam {
	if fl == nil {
		return nil
	}
	var out []testParam
	unnamed := 0
	for _, field := range fl.List {
		variadic := false
		typExpr := field.Type
		if ell, ok := typExpr.(*ast.Ellipsis); ok {
			variadic = true
			typExpr = ell.Elt
		}
		typ := exprString(fset, typExpr)
		if len(field.Names) == 0 {
			out = append(out, testParam{fmt.Sprintf("in%d", unnamed), typ, variadic})
			unnamed++
			continue
		}
		for _, n := range field.Names {
			name := n.Name
			if name == "_" {
				name = fmt.Sprintf("in%d", unnamed)
				unnamed++
			}
			out = append(out, testParam{name, typ, variadic})
		}
	}
	return out
}
