package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fatih/astrewrite"
	"golang.org/x/mod/modfile"
)

// TODO investigate https://pkg.go.dev/golang.org/x/tools/go/ast/astutil#Apply

const mainName = "MainFunction"

func getModuleName(filename string) (string, error) {
	goModBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return "", err
	}

	modName := modfile.ModulePath(goModBytes)

	return modName, nil
}

type mainPackage struct {
	command     string
	importPath  string
	packageName string
	outputDir   string
	contents    map[string][]byte
}

type combiner struct {
	serviceDir string
	module     string
	outputDir  string
	packages   map[string]*mainPackage
}

func newCombiner(serviceDir string) (*combiner, error) {
	serviceDir, err := filepath.Abs(serviceDir)
	if err != nil {
		return nil, err
	}

	module, err := getModuleName(filepath.Join(serviceDir, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("failed to get module name: %w", err)
	}

	return &combiner{
		serviceDir: serviceDir,
		module:     module,
		packages:   make(map[string]*mainPackage),
		outputDir:  filepath.Join("internal", "combiner"),
	}, nil
}

func (c *combiner) collect(dir string) error {
	walkFn := func(in string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		if !strings.HasSuffix(in, ".go") || strings.HasSuffix(in, "_test.go") {
			return nil
		}

		ok, err := isMain(in)
		if err != nil {
			return err
		}

		if !ok {
			return nil
		}

		dirName := filepath.Dir(in)

		m := c.packages[dirName]
		if m == nil {
			importPath := path.Join(c.module, filepath.ToSlash(c.outputDir), strings.TrimPrefix(dirName, c.serviceDir))

			m = &mainPackage{
				command:     filepath.Base(dirName),
				importPath:  importPath,
				contents:    make(map[string][]byte),
				packageName: strings.NewReplacer("-", "_").Replace(filepath.Base(dirName)),
				outputDir:   filepath.Join(c.outputDir, strings.TrimPrefix(dirName, c.serviceDir)),
			}

			fmt.Printf("%#v\n", m)

			c.packages[dirName] = m
		}

		data, err := parseAndReplace(m.packageName, in)
		if err != nil {
			return err
		}

		m.contents[in] = data

		return nil
	}

	return filepath.Walk(filepath.Join(c.serviceDir, dir), walkFn)
}

func (c *combiner) output() error {
	var outputs []*mainPackage

	for _, m := range c.packages {
		outputs = append(outputs, m)
		outDir := filepath.Join(c.serviceDir, m.outputDir)
		if err := os.MkdirAll(outDir, 0755); err != nil {
			return err
		}

		for file, data := range m.contents {
			filename := filepath.Join(outDir, filepath.Base(file))

			if err := ioutil.WriteFile(filename, data, 0644); err != nil {
				return err
			}
		}
	}

	sort.Slice(outputs, func(i, j int) bool {
		return outputs[i].importPath < outputs[j].importPath
	})

	var buf bytes.Buffer
	_, _ = buf.WriteString("package main\nimport (\n\"os\"\n\"fmt\"\n\"filepath\"\n\n")

	for _, m := range outputs {
		_, _ = fmt.Fprintf(&buf, "%s %q\n", m.packageName, m.importPath)
	}

	_, _ = buf.WriteString(`)

func main() {
    name := filepath.Base(os.Args[0])

    switch name {
`)

	for _, m := range outputs {
		_, _ = fmt.Fprintf(&buf, "case %q:\n%s.%s()\n", m.command, m.packageName, mainName)
	}

	_, _ = buf.WriteString(`
default:
  fmt.Fprintf(os.Stderr, "unknown command %s", name)
  os.Exit(11)
}
}
`)

	fmt.Println(buf.String())

	fset := token.NewFileSet()
	mainAST, err := parser.ParseFile(fset, "main.go", buf.Bytes(), parser.ParseComments)
	if err != nil {
		return err
	}

	buf.Reset()
	if err := format.Node(&buf, fset, mainAST); err != nil {
		return fmt.Errorf("failed to format code: %w", err)
	}

	fmt.Println(buf.String())
	return nil
}

func isMain(filename string) (bool, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return false, err
	}

	fset := token.NewFileSet()

	fileAST, err := parser.ParseFile(fset, filename, data, parser.PackageClauseOnly)
	if err != nil {
		return false, fmt.Errorf("failed to parse %s %w", filename, err)
	}

	return fileAST.Name.Name == "main", nil
}

func parseAndReplace(packageName string, filename string) ([]byte, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	oldAST, err := parser.ParseFile(fset, filename, data, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s %w", filename, err)
	}

	t := transform{
		packageName: packageName,
	}

	newAST := astrewrite.Walk(oldAST, t.visitor)

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, newAST); err != nil {
		return nil, fmt.Errorf("failed to format new code: %w", err)
	}

	return buf.Bytes(), nil
}

func main() {
	c, err := newCombiner("/Volumes/CaseSensitive/login/")
	if err != nil {
		log.Fatal(err)
	}

	if err := c.collect("cmd"); err != nil {
		log.Fatal(err)
	}

	if err := c.output(); err != nil {
		log.Fatal(err)
	}
}

type transform struct {
	packageName string
}

func (t *transform) visitor(n ast.Node) (ast.Node, bool) {
	switch v := n.(type) {
	case *ast.File:
		return t.handleFile(v)
	case *ast.FuncDecl:
		return handleFuncDecl(v)
	default:
		return n, true
	}
}

func (t *transform) handleFile(f *ast.File) (ast.Node, bool) {
	if f.Name.Name != "main" {
		return f, false
	}

	f.Name.Name = t.packageName

	return f, true

}

func handleFuncDecl(fd *ast.FuncDecl) (ast.Node, bool) {
	if fd.Recv != nil {
		return fd, false
	}

	if fd.Name.Name != "main" {
		return fd, false
	}

	fd.Name.Name = mainName

	return fd, false
}
