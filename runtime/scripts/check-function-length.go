package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type function struct {
	path   string
	line   int
	name   string
	length int
}

func main() {
	root := flag.String("root", ".", "runtime source root")
	maxLines := flag.Int("max", 80, "maximum function length")
	exceptionsPath := flag.String("exceptions", "", "shared exception file")
	report := flag.Bool("report", false, "print every function without failing")
	flag.Parse()

	exceptions, err := readExceptions(*exceptionsPath, "go")
	if err != nil {
		fatal(err)
	}
	functions, err := findFunctions(*root)
	if err != nil {
		fatal(err)
	}

	sort.Slice(functions, func(i, j int) bool {
		if functions[i].length != functions[j].length {
			return functions[i].length > functions[j].length
		}
		if functions[i].path != functions[j].path {
			return functions[i].path < functions[j].path
		}
		return functions[i].line < functions[j].line
	})

	violations := 0
	for _, fn := range functions {
		exceptionLimit, excepted := exceptions[fn.path+"\t"+fn.name]
		if !*report && (fn.length <= *maxLines || excepted && fn.length <= exceptionLimit) {
			continue
		}
		fmt.Printf("%s:%d:%s:%d\n", fn.path, fn.line, fn.name, fn.length)
		if fn.length > *maxLines && (!excepted || fn.length > exceptionLimit) {
			violations++
		}
	}
	if !*report && violations > 0 {
		os.Exit(1)
	}
}

func readExceptions(path, language string) (map[string]int, error) {
	exceptions := make(map[string]int)
	if path == "" {
		return exceptions, nil
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return exceptions, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 {
			return nil, fmt.Errorf("%s: expected '<language> <path> <function> <current-lines>'", path)
		}
		if fields[0] == language {
			currentLines, err := strconv.Atoi(fields[3])
			if err != nil || currentLines <= 0 {
				return nil, fmt.Errorf("%s: invalid function length %q", path, fields[3])
			}
			exceptions[fields[1]+"\t"+fields[2]] = currentLines
		}
	}
	return exceptions, scanner.Err()
}

func findFunctions(root string) ([]function, error) {
	var functions []function
	for _, sourceRoot := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, sourceRoot), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" || entry.Name() == "protoschema" {
					return filepath.SkipDir
				}
				return nil
			}
			if !isHandwrittenGo(path) {
				return nil
			}
			found, err := functionsInFile(root, path)
			if err != nil {
				return err
			}
			functions = append(functions, found...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return functions, nil
}

func isHandwrittenGo(path string) bool {
	name := filepath.Base(path)
	return strings.HasSuffix(name, ".go") &&
		!strings.HasSuffix(name, "_test.go") &&
		!strings.HasSuffix(name, "_generated.go") &&
		!strings.Contains(name, ".generated.")
}

func functionsInFile(root, path string) ([]function, error) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		return nil, err
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil {
		return nil, err
	}
	relative = filepath.ToSlash(filepath.Join(filepath.Base(absoluteRoot), relative))

	var functions []function
	var stack []ast.Node
	ast.Inspect(parsed, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		var parent ast.Node
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		stack = append(stack, node)

		switch fn := node.(type) {
		case *ast.FuncDecl:
			functions = append(functions, makeFunction(fileSet, relative, functionName(fn), fn.Pos(), fn.End()))
		case *ast.FuncLit:
			line := fileSet.Position(fn.Pos()).Line
			functions = append(functions, makeFunction(fileSet, relative, literalName(parent, line), fn.Pos(), fn.End()))
		}
		return true
	})
	return functions, nil
}

func makeFunction(fileSet *token.FileSet, path, name string, start, end token.Pos) function {
	startLine := fileSet.Position(start).Line
	endLine := fileSet.Position(end).Line
	return function{path: path, line: startLine, name: name, length: endLine - startLine + 1}
}

func functionName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return expressionName(fn.Recv.List[0].Type) + "." + fn.Name.Name
}

func literalName(parent ast.Node, line int) string {
	switch node := parent.(type) {
	case *ast.ValueSpec:
		if len(node.Names) > 0 {
			return node.Names[0].Name
		}
	case *ast.AssignStmt:
		if len(node.Lhs) > 0 {
			return expressionName(node.Lhs[0])
		}
	case *ast.KeyValueExpr:
		return expressionName(node.Key)
	}
	return fmt.Sprintf("<literal@%d>", line)
}

func expressionName(expression ast.Expr) string {
	switch node := expression.(type) {
	case *ast.Ident:
		return node.Name
	case *ast.SelectorExpr:
		return expressionName(node.X) + "." + node.Sel.Name
	case *ast.StarExpr:
		return expressionName(node.X)
	case *ast.IndexExpr:
		return expressionName(node.X)
	case *ast.IndexListExpr:
		return expressionName(node.X)
	}
	return "<expression>"
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
