package reviewgraph

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompilerGraphSeparatesCallsFromDynamicTargets(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.test/review\n\ngo 1.25\n",
		"review.go": `package review
import "fmt"
type I interface { M() }
type S struct { I }
type T struct {}
func (T) M() {}
func direct() {}
func use(i I, s S, t T, value func()) {
 direct(); t.M(); fmt.Println("observed static call")
 i.M(); s.M(); value()
 _ = func() { direct() }
}
`,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"}} {
		if _, err := run(context.Background(), root, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	g, err := Build(context.Background(), root, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]Node{}
	kinds := map[string]int{}
	for _, n := range g.Nodes {
		nodes[n.ID] = n
		kinds[n.Kind]++
	}
	if kinds["package"] != 1 || kinds["file"] != 1 || kinds["type"] != 3 || kinds["function"] != 3 {
		t.Fatalf("structure: %v", kinds)
	}
	calls := []string{}
	for _, e := range g.Edges {
		if _, ok := nodes[e.From]; !ok {
			t.Fatalf("dangling source: %v", e)
		}
		if _, ok := nodes[e.To]; !ok {
			t.Fatalf("dangling target: %v", e)
		}
		if e.Kind == "static_call" {
			calls = append(calls, nodes[e.To].Name)
		}
	}
	if len(calls) != 3 {
		t.Fatalf("dynamic/closure calls were promoted: %v", calls)
	}
	for _, suffix := range []string{".direct()", ").M()", ".Println("} {
		found := false
		for _, call := range calls {
			found = found || strings.Contains(call, suffix)
		}
		if !found {
			t.Fatalf("missing %s in %v", suffix, calls)
		}
	}
	if len(g.Unknowns) != 4 {
		t.Fatalf("missing dynamic/closure gaps: %+v", g.Unknowns)
	}
	if len(g.Sources) != 1 || g.Sources[0].SHA256 != digest([]byte(files["review.go"])) {
		t.Fatalf("source binding: %+v", g.Sources)
	}
	if _, err := Build(context.Background(), root, nil); err == nil {
		t.Fatal("unselected package scope accepted")
	}
	t.Run("target architecture controls type sizes", func(t *testing.T) {
		t.Setenv("GOOS", "linux")
		t.Setenv("GOARCH", "386")
		t.Setenv("CGO_ENABLED", "0")
		data := []byte("package review\nimport \"unsafe\"\nvar _ [4-unsafe.Sizeof(uintptr(0))]byte\n")
		if err := os.WriteFile(filepath.Join(root, "size.go"), data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Build(context.Background(), root, []string{"."}); err != nil {
			t.Fatalf("32-bit compiler input was checked with host sizes: %v", err)
		}
	})
}
