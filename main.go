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
	"gopkg.in/alecthomas/kingpin.v2"
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
	include    []string
}

func newCombiner(serviceDir string, outputDir string, include []string) (*combiner, error) {
	serviceDir, err := filepath.Abs(serviceDir)
	if err != nil {
		return nil, err
	}

	outputDir, err = filepath.Abs(filepath.Join(serviceDir, outputDir))
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
		outputDir:  outputDir,
		include:    include,
	}, nil
}

var alwaysIgnore = []string{
	".git",
	"vendor",
	".idea",
	".github",
}

func (c *combiner) collect() error {
	counter := 0

	walkFn := func(fullPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relativePath := strings.TrimPrefix(strings.TrimPrefix(fullPath, c.serviceDir), "/")

		if info.IsDir() {
			for _, ignore := range alwaysIgnore {
				if ignore == relativePath {
					return filepath.SkipDir
				}
			}

			if fullPath == c.outputDir {
				return filepath.SkipDir
			}

			return nil
		}

		if len(c.include) > 0 && relativePath != "" {
			found := false

			for _, d := range c.include {
				if strings.HasPrefix(relativePath, d+"/") {
					found = true
					break
				}
			}

			if !found {
				return nil
			}
		}

		if !strings.HasSuffix(fullPath, ".go") || strings.HasSuffix(fullPath, "_test.go") {
			return nil
		}

		ok, err := isMain(fullPath)
		if err != nil {
			return err
		}

		if !ok {
			return nil
		}

		dirName := filepath.Dir(relativePath)

		m := c.packages[dirName]
		if m == nil {
			replacer := strings.NewReplacer("-", "_", "/", "_")
			packageName := replacer.Replace(dirName)
			importPath := path.Join(c.module, strings.TrimPrefix(c.outputDir, c.serviceDir), packageName)

			m = &mainPackage{
				command:     filepath.Base(dirName),
				importPath:  importPath,
				contents:    make(map[string][]byte),
				packageName: packageName,
				outputDir:   filepath.Join(c.outputDir, packageName),
			}

			counter++

			c.packages[dirName] = m
		}

		data, err := parseAndReplace(m.packageName, fullPath)
		if err != nil {
			return err
		}

		m.contents[fullPath] = data

		return nil
	}

	return filepath.Walk(c.serviceDir, walkFn)
}

func (c *combiner) output() error {
	var outputs []*mainPackage

	for _, m := range c.packages {
		outputs = append(outputs, m)
		if err := os.MkdirAll(m.outputDir, 0755); err != nil {
			return err
		}

		for file, data := range m.contents {
			filename := filepath.Join(m.outputDir, filepath.Base(file))

			if err := ioutil.WriteFile(filename, data, 0644); err != nil {
				return err
			}
		}
	}

	sort.Slice(outputs, func(i, j int) bool {
		return outputs[i].importPath < outputs[j].importPath
	})

	var buf bytes.Buffer
	_, _ = buf.WriteString("package main\nimport (\n\"os\"\n\"fmt\"\n\"path/filepath\"\n\n")

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
  fmt.Fprintf(os.Stderr, "unknown command %s\n", name)
  os.Exit(11)
}
}
`)

	fset := token.NewFileSet()
	mainAST, err := parser.ParseFile(fset, "main.go", buf.Bytes(), parser.ParseComments)
	if err != nil {
		return err
	}

	buf.Reset()
	if err := format.Node(&buf, fset, mainAST); err != nil {
		return fmt.Errorf("failed to format code: %w", err)
	}

	if err := os.MkdirAll(c.outputDir, 0755); err != nil {
		return err
	}

	filename := filepath.Join(c.outputDir, "main.go")

	return ioutil.WriteFile(filename, buf.Bytes(), 0644)
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
	log.SetFlags(0)

	input := kingpin.Flag("input", "input directory").Default(".").ExistingDir()
	output := kingpin.Flag("output", "out directory relative to input").Default("cmd/combined").String()
	include := kingpin.Flag("include", "if set, only include these dirctories").Default().Strings()

	kingpin.Parse()

	c, err := newCombiner(*input, *output, *include)

	if err != nil {
		log.Fatal(err)
	}

	if err := c.collect(); err != nil {
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
