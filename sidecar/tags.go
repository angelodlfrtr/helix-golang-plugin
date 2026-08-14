package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// cmdTags adds, removes, or clears struct field tags for the struct under the
// cursor, rewriting the file in place (the editor reloads afterwards).
func cmdTags(argv []string) error {
	fs := flag.NewFlagSet("tags", flag.ContinueOnError)
	file := fs.String("file", "", "source file")
	line := fs.Int("line", 0, "1-based cursor line (inside a struct)")
	op := fs.String("op", "add", "add | remove | clear")
	tags := fs.String("tags", "json", "comma separated tag keys")
	transform := fs.String("transform", "snakecase", "snakecase | camelcase | lispcase | pascalcase | keep")
	options := fs.String("options", "", "comma separated tag options appended to added tags (e.g. omitempty)")
	skipUnexported := fs.Bool("skip-unexported", true, "leave unexported fields untouched")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *file == "" || *line <= 0 {
		return fmt.Errorf("tags: -file and -line are required")
	}
	keys := splitList(*tags)
	if len(keys) == 0 && *op != "clear" {
		return fmt.Errorf("tags: -tags is required for op %q", *op)
	}

	src, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, *file, src, parser.ParseComments)
	if err != nil {
		return err
	}

	st, topDecl := structAtLine(fset, f, *line)
	if st == nil {
		return fmt.Errorf("no struct found at line %d", *line)
	}

	changed := 0
	for _, field := range st.Fields.List {
		name := fieldName(field)
		if name == "" {
			continue
		}
		if *skipUnexported && !unicode.IsUpper([]rune(name)[0]) {
			continue
		}
		switch *op {
		case "add":
			cur := parseTag(field.Tag)
			for _, k := range keys {
				val := transformName(name, *transform)
				if *options != "" {
					val += "," + strings.Join(splitList(*options), ",")
				}
				cur = setTagKey(cur, k, val)
			}
			setFieldTag(field, cur)
			changed++
		case "remove":
			cur := parseTag(field.Tag)
			for _, k := range keys {
				cur = delTagKey(cur, k)
			}
			setFieldTag(field, cur)
			changed++
		case "clear":
			setFieldTag(field, nil)
			changed++
		default:
			return fmt.Errorf("tags: unknown op %q", *op)
		}
	}
	if changed == 0 {
		return fmt.Errorf("no eligible fields in struct at line %d", *line)
	}

	// Re-print only the top-level declaration containing the struct and
	// splice it back, so the rest of the file keeps its exact formatting.
	start := fset.Position(topDecl.Pos()).Offset
	end := fset.Position(topDecl.End()).Offset
	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	if err := cfg.Fprint(&buf, fset, &printer.CommentedNode{Node: topDecl, Comments: f.Comments}); err != nil {
		return err
	}
	var out bytes.Buffer
	out.Write(src[:start])
	out.Write(buf.Bytes())
	out.Write(src[end:])
	formatted, err := format.Source(out.Bytes())
	if err != nil {
		// Should not happen; fall back to the unformatted splice.
		formatted = out.Bytes()
	}
	info, err := os.Stat(*file)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = info.Mode()
	}
	if err := writeFileAtomic(*file, formatted, mode); err != nil {
		return err
	}
	emit(map[string]any{"ok": true, "fields": changed})
	return nil
}

// structAtLine returns the innermost struct type whose extent contains line,
// along with the top-level declaration containing it.
func structAtLine(fset *token.FileSet, f *ast.File, line int) (*ast.StructType, ast.Decl) {
	var best *ast.StructType
	var bestDecl ast.Decl
	for _, d := range f.Decls {
		ds := fset.Position(d.Pos()).Line
		de := fset.Position(d.End()).Line
		if line < ds || line > de {
			continue
		}
		ast.Inspect(d, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			s := fset.Position(st.Pos()).Line
			e := fset.Position(st.End()).Line
			if line >= s && line <= e {
				best = st // keep innermost: later (deeper) matches overwrite
				bestDecl = d
			}
			return true
		})
	}
	return best, bestDecl
}

func fieldName(field *ast.Field) string {
	if len(field.Names) > 0 {
		return field.Names[0].Name
	}
	// Embedded field: use the type's base name.
	t := field.Type
	for {
		switch v := t.(type) {
		case *ast.StarExpr:
			t = v.X
		case *ast.SelectorExpr:
			return v.Sel.Name
		case *ast.Ident:
			return v.Name
		default:
			return ""
		}
	}
}

// tagEntry preserves the order of keys in a struct tag.
type tagEntry struct {
	key   string
	value string
}

func parseTag(tag *ast.BasicLit) []tagEntry {
	if tag == nil {
		return nil
	}
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		return nil
	}
	var entries []tagEntry
	// Mirrors reflect.StructTag.Lookup's scanning rules.
	for raw != "" {
		i := 0
		for i < len(raw) && raw[i] == ' ' {
			i++
		}
		raw = raw[i:]
		if raw == "" {
			break
		}
		i = 0
		for i < len(raw) && raw[i] > ' ' && raw[i] != ':' && raw[i] != '"' && raw[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(raw) || raw[i] != ':' || raw[i+1] != '"' {
			break
		}
		key := raw[:i]
		raw = raw[i+1:]
		i = 1
		for i < len(raw) && raw[i] != '"' {
			if raw[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(raw) {
			break
		}
		quoted := raw[:i+1]
		raw = raw[i+1:]
		value, err := strconv.Unquote(quoted)
		if err != nil {
			break
		}
		entries = append(entries, tagEntry{key, value})
	}
	return entries
}

func setTagKey(entries []tagEntry, key, value string) []tagEntry {
	for i := range entries {
		if entries[i].key == key {
			entries[i].value = value
			return entries
		}
	}
	return append(entries, tagEntry{key, value})
}

func delTagKey(entries []tagEntry, key string) []tagEntry {
	out := entries[:0]
	for _, e := range entries {
		if e.key != key {
			out = append(out, e)
		}
	}
	return out
}

func setFieldTag(field *ast.Field, entries []tagEntry) {
	if len(entries) == 0 {
		field.Tag = nil
		return
	}
	var parts []string
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf("%s:%q", e.key, e.value))
	}
	lit := "`" + strings.Join(parts, " ") + "`"
	if field.Tag == nil {
		field.Tag = &ast.BasicLit{Kind: token.STRING, Value: lit}
	} else {
		field.Tag.Value = lit
	}
}

// transformName converts a Go field name to the requested tag-value style.
func transformName(name, transform string) string {
	words := splitCamel(name)
	switch transform {
	case "keep":
		return name
	case "snakecase":
		return strings.ToLower(strings.Join(words, "_"))
	case "lispcase":
		return strings.ToLower(strings.Join(words, "-"))
	case "camelcase":
		for i := 1; i < len(words); i++ {
			words[i] = titleWord(words[i])
		}
		if len(words) > 0 {
			words[0] = strings.ToLower(words[0])
		}
		return strings.Join(words, "")
	case "pascalcase":
		for i := range words {
			words[i] = titleWord(words[i])
		}
		return strings.Join(words, "")
	default:
		return strings.ToLower(strings.Join(words, "_"))
	}
}

func titleWord(w string) string {
	if w == "" {
		return w
	}
	r := []rune(w)
	return string(unicode.ToUpper(r[0])) + strings.ToLower(string(r[1:]))
}

// splitCamel splits FooBarID into ["Foo", "Bar", "ID"], keeping initialisms
// together (HTTPServer -> ["HTTP", "Server"]).
func splitCamel(s string) []string {
	var words []string
	runes := []rune(s)
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		boundary := false
		if unicode.IsUpper(cur) && !unicode.IsUpper(prev) && prev != '_' {
			boundary = true
		}
		// End of an initialism run: "HTTPServer" -> boundary before 'S' of Server.
		if unicode.IsUpper(prev) && unicode.IsUpper(cur) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
			boundary = true
		}
		if cur == '_' {
			words = append(words, string(runes[start:i]))
			start = i + 1
			continue
		}
		if boundary {
			words = append(words, string(runes[start:i]))
			start = i
		}
	}
	if start < len(runes) {
		words = append(words, string(runes[start:]))
	}
	var out []string
	for _, w := range words {
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
