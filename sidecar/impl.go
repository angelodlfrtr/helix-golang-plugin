package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// cmdImpl generates method stubs implementing an interface for a receiver,
// in the spirit of josharian/impl but with no external dependencies: package
// export data is resolved through `go list -export`, so it works for the
// stdlib, the current module, and module-cache dependencies alike.
func cmdImpl(argv []string) error {
	fs := flag.NewFlagSet("impl", flag.ContinueOnError)
	file := fs.String("file", "", "target source file (provides package context)")
	recv := fs.String("recv", "", `receiver, e.g. "s *Server"`)
	iface := fs.String("iface", "", `interface, e.g. "io.Reader", "Handler", "golang.org/x/net/websocket.Codec"`)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *file == "" || *recv == "" || *iface == "" {
		return fmt.Errorf("impl: -file, -recv and -iface are required")
	}
	recvDecl := strings.TrimSpace(*recv)
	recvBase := receiverBaseType(recvDecl)
	if recvBase == "" {
		return fmt.Errorf(`impl: cannot parse receiver %q (expected e.g. "s *Server")`, recvDecl)
	}

	dir := filepath.Dir(*file)
	fset := token.NewFileSet()
	targetFile, err := parser.ParseFile(fset, *file, nil, parser.SkipObjectResolution)
	if err != nil {
		return err
	}

	imp := newExportImporter(fset, dir)

	pkgPath, name := splitIfaceSpec(*iface, targetFile)
	var scope *types.Scope
	var selfPath string
	if out, _, err := runGo(dir, "list", "-f", "{{.ImportPath}}", "."); err == nil {
		selfPath = strings.TrimSpace(out)
	}
	if pkgPath == "" {
		// Interface in the current package: type-check the package source.
		pkg, err := checkLocalPackage(fset, dir, imp)
		if err != nil {
			return fmt.Errorf("impl: type-checking %s: %v", dir, err)
		}
		scope = pkg.Scope()
	} else {
		pkg, err := imp.Import(pkgPath)
		if err != nil {
			return fmt.Errorf("impl: importing %q: %v", pkgPath, err)
		}
		scope = pkg.Scope()
	}

	obj := scope.Lookup(name)
	if obj == nil {
		return fmt.Errorf("impl: %s not found in package %q", name, orSelf(pkgPath))
	}
	named, ok := obj.Type().(*types.Named)
	if ok && named.TypeParams().Len() > 0 {
		return fmt.Errorf("impl: generic interfaces are not supported yet")
	}
	ifaceType, ok := obj.Type().Underlying().(*types.Interface)
	if !ok {
		return fmt.Errorf("impl: %s is not an interface", *iface)
	}

	existing := existingMethods(dir, recvBase)
	qual := func(p *types.Package) string {
		if p == nil || p.Path() == selfPath {
			return ""
		}
		return p.Name()
	}

	var b strings.Builder
	generated := 0
	for i := 0; i < ifaceType.NumMethods(); i++ {
		m := ifaceType.Method(i)
		if existing[m.Name()] {
			continue
		}
		sig := m.Type().(*types.Signature)
		fmt.Fprintf(&b, "func (%s) %s%s%s {\n\tpanic(\"not implemented\") // TODO: Implement\n}\n\n",
			recvDecl, m.Name(), formatParams(sig, qual), formatResults(sig, qual))
		generated++
	}
	if generated == 0 {
		return fmt.Errorf("impl: %s already implements all %d method(s) of %s", recvBase, ifaceType.NumMethods(), *iface)
	}
	emit(map[string]any{"code": strings.TrimRight(b.String(), "\n") + "\n", "methods": generated})
	return nil
}

func orSelf(p string) string {
	if p == "" {
		return "(current package)"
	}
	return p
}

// receiverBaseType extracts "Server" from "s *Server" / "s Server" / "*Server".
func receiverBaseType(recv string) string {
	fields := strings.Fields(recv)
	if len(fields) == 0 {
		return ""
	}
	t := fields[len(fields)-1]
	t = strings.TrimPrefix(t, "*")
	if idx := strings.IndexByte(t, '['); idx >= 0 {
		t = t[:idx]
	}
	if t == "" || !isIdent(t) {
		return ""
	}
	return t
}

func isIdent(s string) bool {
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return s != ""
}

// splitIfaceSpec resolves an interface spec to (importPath, name).
// importPath is "" for the current package. A bare package name selector
// ("io.Reader") is resolved through the target file's imports first.
func splitIfaceSpec(spec string, f *ast.File) (string, string) {
	idx := strings.LastIndex(spec, ".")
	if idx < 0 {
		return "", spec
	}
	pkgPart, name := spec[:idx], spec[idx+1:]
	if strings.Contains(pkgPart, "/") {
		return pkgPart, name
	}
	// Match against the file's imports by alias or base name.
	for _, im := range f.Imports {
		path := strings.Trim(im.Path.Value, `"`)
		if im.Name != nil {
			if im.Name.Name == pkgPart {
				return path, name
			}
			continue
		}
		if filepath.Base(path) == pkgPart {
			return path, name
		}
	}
	// Not imported: assume the selector is the import path itself ("io.Reader").
	return pkgPart, name
}

// checkLocalPackage type-checks the non-test files of dir, best-effort.
func checkLocalPackage(fset *token.FileSet, dir string, imp types.Importer) (*types.Package, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no Go files in %s", dir)
	}
	conf := types.Config{
		Importer:         imp,
		FakeImportC:      true,
		IgnoreFuncBodies: true,
		Error:            func(error) {}, // collect nothing; use best-effort package
	}
	pkg, _ := conf.Check(dir, fset, files, nil)
	if pkg == nil {
		return nil, fmt.Errorf("type check produced no package")
	}
	return pkg, nil
}

// existingMethods lists method names already defined on the receiver base
// type anywhere in the package directory.
func existingMethods(dir, recvBase string) map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			base := fd.Recv.List[0].Type
			if se, ok := base.(*ast.StarExpr); ok {
				base = se.X
			}
			// Strip generic receiver type arguments: T[K] -> T.
			if ie, ok := base.(*ast.IndexExpr); ok {
				base = ie.X
			}
			if id, ok := base.(*ast.Ident); ok && id.Name == recvBase {
				out[fd.Name.Name] = true
			}
		}
	}
	return out
}

func formatParams(sig *types.Signature, qual types.Qualifier) string {
	params := sig.Params()
	var parts []string
	for i := 0; i < params.Len(); i++ {
		v := params.At(i)
		t := types.TypeString(v.Type(), qual)
		if sig.Variadic() && i == params.Len()-1 {
			t = "..." + strings.TrimPrefix(t, "[]")
		}
		if v.Name() != "" {
			parts = append(parts, v.Name()+" "+t)
		} else {
			parts = append(parts, t)
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func formatResults(sig *types.Signature, qual types.Qualifier) string {
	results := sig.Results()
	if results.Len() == 0 {
		return ""
	}
	var parts []string
	named := false
	for i := 0; i < results.Len(); i++ {
		v := results.At(i)
		t := types.TypeString(v.Type(), qual)
		if v.Name() != "" {
			named = true
			parts = append(parts, v.Name()+" "+t)
		} else {
			parts = append(parts, t)
		}
	}
	if len(parts) == 1 && !named {
		return " " + parts[0]
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// exportImporter resolves packages through `go list -export`, reading the
// compiler export data with the stdlib gc importer.
type exportImporter struct {
	dir      string
	delegate types.ImporterFrom
}

func newExportImporter(fset *token.FileSet, dir string) *exportImporter {
	ei := &exportImporter{dir: dir}
	lookup := func(path string) (io.ReadCloser, error) {
		out, stderr, err := runGo(dir, "list", "-export", "-f", "{{.Export}}", path)
		if err != nil {
			return nil, fmt.Errorf("go list -export %s: %v: %s", path, err, strings.TrimSpace(stderr))
		}
		exportFile := strings.TrimSpace(out)
		if exportFile == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(exportFile)
	}
	ei.delegate = importer.ForCompiler(fset, "gc", lookup).(types.ImporterFrom)
	return ei
}

func (ei *exportImporter) Import(path string) (*types.Package, error) {
	return ei.ImportFrom(path, ei.dir, 0)
}

func (ei *exportImporter) ImportFrom(path, dir string, mode types.ImportMode) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	return ei.delegate.ImportFrom(path, dir, mode)
}
