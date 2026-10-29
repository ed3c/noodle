// Package reviewgraph projects selected Go compiler inputs into review data.
// It does not execute tests, accept candidates, or infer dynamic call targets.
package reviewgraph

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type Source struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Node struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Package string `json:"package"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line"`
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	File string `json:"file"`
	Line int    `json:"line"`
}

type Unknown struct {
	From   string `json:"from"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

type Graph struct {
	Schema           int             `json:"schema"`
	Analyzer         string          `json:"analyzer"`
	Root             string          `json:"root"`
	HEAD             string          `json:"head"`
	BuildEnvironment json.RawMessage `json:"build_environment"`
	Patterns         []string        `json:"patterns"`
	Sources          []Source        `json:"sources"`
	Nodes            []Node          `json:"nodes"`
	Edges            []Edge          `json:"edges"`
	Unknowns         []Unknown       `json:"unknowns"`
	Limitations      []string        `json:"limitations"`
	targets          map[string]Node
	sizes            types.Sizes
}

type listedPackage struct {
	Dir, ImportPath, Export           string
	GoFiles, CgoFiles, IgnoredGoFiles []string
	DepOnly                           bool
}

func run(ctx context.Context, root, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %v: %w: %s", name, args, err, stderr.String())
	}
	return data, nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func id(kind, key string) string { return kind + "-" + digest([]byte(key)) }

func Build(ctx context.Context, root string, patterns []string) (*Graph, error) {
	if len(patterns) == 0 {
		return nil, fmt.Errorf("selected package scope is missing")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	head, err := run(ctx, root, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	env, err := run(ctx, root, "go", "env", "-json", "GOOS", "GOARCH", "GOVERSION", "CGO_ENABLED", "GOFLAGS")
	if err != nil {
		return nil, err
	}
	args := append([]string{"list", "-deps", "-export", "-json", "--"}, patterns...)
	data, err := run(ctx, root, "go", args...)
	if err != nil {
		return nil, err
	}
	packages, exports, err := decodePackages(data)
	if err != nil {
		return nil, err
	}
	g := &Graph{Schema: 1, Analyzer: "noodle-go-types-v1", Root: root,
		HEAD: string(bytes.TrimSpace(head)), BuildEnvironment: env, Patterns: patterns,
		Sources: []Source{}, Nodes: []Node{}, Edges: []Edge{}, Unknowns: []Unknown{}, targets: map[string]Node{},
		Limitations: []string{"static source relationships, not runtime traces", "test files excluded", "build-excluded files are not analyzed", "dynamic dispatch and function values remain unknown", "quality metrics are separate original tool evidence"}}
	var buildEnv struct{ GOARCH string }
	if err := json.Unmarshal(env, &buildEnv); err != nil {
		return nil, fmt.Errorf("decode compiler environment: %w", err)
	}
	g.sizes = types.SizesFor("gc", buildEnv.GOARCH)
	if g.sizes == nil {
		return nil, fmt.Errorf("compiler architecture is unsupported: %s", buildEnv.GOARCH)
	}
	g.Limitations = append(g.Limitations, "source hashes bind selected files; module metadata and dependency exports are not a reproducible build snapshot")
	for _, pkg := range packages {
		if err := g.addPackage(pkg, exports); err != nil {
			return nil, err
		}
	}
	for _, n := range g.Nodes {
		delete(g.targets, n.ID)
	}
	for _, n := range g.targets {
		g.Nodes = append(g.Nodes, n)
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	sort.Slice(g.Sources, func(i, j int) bool { return g.Sources[i].Path < g.Sources[j].Path })
	for _, source := range g.Sources {
		current, err := os.ReadFile(filepath.Join(root, source.Path))
		if err != nil || digest(current) != source.SHA256 {
			return nil, fmt.Errorf("source changed during analysis: %s", source.Path)
		}
	}
	currentHead, err := run(ctx, root, "git", "rev-parse", "HEAD")
	if err != nil || !bytes.Equal(head, currentHead) {
		return nil, fmt.Errorf("repository HEAD changed during analysis")
	}
	return g, nil
}

func decodePackages(data []byte) ([]listedPackage, map[string]string, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	selected := []listedPackage{}
	exports := map[string]string{}
	for {
		var p listedPackage
		err := d.Decode(&p)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		exports[p.ImportPath] = p.Export
		if !p.DepOnly {
			selected = append(selected, p)
		}
	}
	if len(selected) == 0 {
		return nil, nil, fmt.Errorf("package scope matched no compiler inputs")
	}
	return selected, exports, nil
}

func (g *Graph) addPackage(p listedPackage, exports map[string]string) error {
	if len(p.CgoFiles) != 0 {
		return fmt.Errorf("package %s requires cgo analysis, unsupported", p.ImportPath)
	}
	fset := token.NewFileSet()
	files := []*ast.File{}
	paths := map[*ast.File]string{}
	for _, name := range p.GoFiles {
		full := filepath.Join(p.Dir, name)
		real, err := filepath.EvalSymlinks(full)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(g.Root, real)
		if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("selected source is outside repository: %s", full)
		}
		data, err := os.ReadFile(real)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, full, data, 0)
		if err != nil {
			return err
		}
		files, paths[file] = append(files, file), filepath.ToSlash(rel)
		g.Sources = append(g.Sources, Source{filepath.ToSlash(rel), digest(data)})
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	config := types.Config{Sizes: g.sizes, Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		archive := exports[path]
		if archive == "" {
			return nil, fmt.Errorf("compiler export missing for %s", path)
		}
		return os.Open(archive)
	})}
	if _, err := config.Check(p.ImportPath, fset, files, info); err != nil {
		return fmt.Errorf("typecheck %s: %w", p.ImportPath, err)
	}
	packageID := id("package", p.ImportPath)
	g.Nodes = append(g.Nodes, Node{ID: packageID, Kind: "package", Name: p.ImportPath, Package: p.ImportPath})
	for _, file := range files {
		g.addFile(p.ImportPath, paths[file], file, fset, info, packageID)
	}
	return nil
}

func objectKey(obj types.Object) string {
	return types.ObjectString(obj, func(p *types.Package) string { return p.Path() })
}

func (g *Graph) addFile(pkg, path string, file *ast.File, fset *token.FileSet, info *types.Info, packageID string) {
	fileID := id("file", pkg+":"+path)
	g.Nodes = append(g.Nodes, Node{ID: fileID, Kind: "file", Name: filepath.Base(path), Package: pkg, File: path, Line: 1, EndLine: fset.Position(file.End()).Line})
	g.Edges = append(g.Edges, Edge{From: packageID, To: fileID, Kind: "contains", File: path, Line: 1})
	ast.Inspect(file, func(n ast.Node) bool {
		var obj types.Object
		kind := ""
		switch n := n.(type) {
		case *ast.TypeSpec:
			obj, kind = info.Defs[n.Name], "type"
		case *ast.FuncDecl:
			obj, kind = info.Defs[n.Name], "function"
		default:
			return true
		}
		if obj == nil {
			return true
		}
		nodeID := id(kind, objectKey(obj))
		if obj.Name() == "init" {
			nodeID = id(kind, pkg+":"+path+":"+fmt.Sprint(fset.Position(n.Pos()).Line))
		}
		line := fset.Position(n.Pos()).Line
		g.Nodes = append(g.Nodes, Node{ID: nodeID, Kind: kind, Name: objectKey(obj), Package: pkg, File: path, Line: line, EndLine: fset.Position(n.End()).Line})
		g.Edges = append(g.Edges, Edge{From: fileID, To: nodeID, Kind: "contains", File: path, Line: line})
		if fn, ok := n.(*ast.FuncDecl); ok && fn.Body != nil {
			g.addCalls(nodeID, path, fn.Body, fset, info)
		}
		return false
	})
}

func callObject(expr ast.Expr, info *types.Info) (types.Object, string) {
	switch x := expr.(type) {
	case *ast.Ident:
		return info.Uses[x], ""
	case *ast.SelectorExpr:
		if sel := info.Selections[x]; sel != nil {
			if _, ok := sel.Recv().Underlying().(*types.Interface); ok {
				return nil, "interface dispatch"
			}
			if sig, ok := sel.Obj().Type().(*types.Signature); ok && sig.Recv() != nil {
				if _, ok := sig.Recv().Type().Underlying().(*types.Interface); ok {
					return nil, "promoted interface dispatch"
				}
			}
		}
		return info.Uses[x.Sel], ""
	case *ast.ParenExpr:
		return callObject(x.X, info)
	case *ast.IndexExpr:
		return callObject(x.X, info)
	case *ast.IndexListExpr:
		return callObject(x.X, info)
	default:
		return nil, "dynamic call target"
	}
}

func (g *Graph) addCalls(from, path string, body *ast.BlockStmt, fset *token.FileSet, info *types.Info) {
	ast.Inspect(body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.FuncLit); ok {
			g.Unknowns = append(g.Unknowns, Unknown{from, path, fset.Position(lit.Pos()).Line, "closure body excluded from enclosing function calls"})
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || info.Types[call.Fun].IsType() {
			return true
		}
		obj, reason := callObject(call.Fun, info)
		line := fset.Position(call.Pos()).Line
		if fn, ok := obj.(*types.Func); ok {
			fn = fn.Origin()
			target := id("function", objectKey(fn))
			g.Edges = append(g.Edges, Edge{from, target, "static_call", path, line})
			g.targets[target] = Node{ID: target, Kind: "external_function", Name: objectKey(fn), Package: fn.Pkg().Path()}
		} else if _, builtin := obj.(*types.Builtin); !builtin {
			if reason == "" {
				reason = "function value target unresolved"
			}
			g.Unknowns = append(g.Unknowns, Unknown{from, path, line, reason})
		}
		return true
	})
}
