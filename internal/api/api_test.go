package api

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
)

// stub is an executor that does nothing, which compiles without buildah or the container store:
// what a proxy to a remote executor needs of this package.
type stub struct{}

var _ Executor = stub{}

func (stub) Capabilities(context.Context) (Capabilities, error)           { return Capabilities{}, nil }
func (stub) Submit(context.Context, Request) (JobID, error)               { return "", nil }
func (stub) Watch(context.Context, JobID, int64, func(Event) error) error { return nil }
func (stub) Cancel(context.Context, JobID) error                          { return nil }
func (stub) Result(context.Context, JobID) (Outcome, error)               { return Outcome{}, nil }
func (stub) Prune(context.Context) (JobID, error)                         { return "", nil }
func (stub) Release(context.Context, JobID) error                         { return nil }

// TestNoHandles checks that no field of a value that crosses the executor boundary, however deep,
// has a type that names a host-local resource or can't cross as JSON: a file, a reader, a function, a channel,
// a pointer into memory or an interface, other than any as a trace argument's value.
// A path is a string, which the test can't tell from any other, so it also refuses field names that suggest one.
func TestNoHandles(t *testing.T) {
	values := []any{Capabilities{}, Request{}, GraphRequest{}, PushRequest{}, ExportRequest{}, Event{}, Outcome{},
		Result{}, Manifest{}, Pushed{}, Exported{}, Pruned{}, Failure{}}
	seen := map[reflect.Type]bool{}
	var problems []string
	for _, v := range values {
		checkType(reflect.TypeOf(v), reflect.TypeOf(v).Name(), seen, &problems)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// TestCheckType checks that checkType finds an interface wherever it is, but as a trace argument's value,
// even where its type was checked before at a path that allows it.
func TestCheckType(t *testing.T) {
	type value struct {
		Spans []tracing.Span // whose Args are map[string]any, which is allowed there
		Extra map[string]any
		Other []any
	}
	var problems []string
	checkType(reflect.TypeOf(value{}), "value", map[reflect.Type]bool{}, &problems)
	want := []string{
		"value.Extra[] is an interface, interface {}, which could hold anything",
		"value.Other[] is an interface, interface {}, which could hold anything",
	}
	if !slices.Equal(problems, want) {
		t.Errorf("problems = %q, want %q", problems, want)
	}
}

// checkType checks the type typ, reached at path, and the types of its fields and elements,
// appending what it finds to problems.
// Struct types are checked once, as seen records, since their fields' paths start afresh;
// any other type is checked wherever it is reached, since whether an interface is allowed depends on the path.
func checkType(typ reflect.Type, path string, seen map[reflect.Type]bool, problems *[]string) {
	switch typ.Kind() {
	case reflect.Func, reflect.Chan, reflect.UnsafePointer, reflect.Uintptr, reflect.Complex64, reflect.Complex128:
		*problems = append(*problems, fmt.Sprintf("%s has type %s, which can't cross the executor boundary", path, typ))
	case reflect.Interface:
		if !strings.HasSuffix(path, ".Args[]") {
			*problems = append(*problems, fmt.Sprintf("%s is an interface, %s, which could hold anything", path, typ))
		}
	case reflect.Pointer, reflect.Slice, reflect.Array:
		checkType(typ.Elem(), path+"[]", seen, problems)
	case reflect.Map:
		checkType(typ.Key(), path+"{}", seen, problems)
		checkType(typ.Elem(), path+"[]", seen, problems)
	case reflect.Struct:
		if seen[typ] {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			f := typ.Field(i)
			name := fmt.Sprintf("%s.%s", path, f.Name)
			for _, hint := range []string{"Path", "Dir", "File", "Fd", "Handle"} {
				// Files is a tree's content reference, not a file.
				if strings.Contains(f.Name, hint) && f.Name != "Files" {
					*problems = append(*problems, fmt.Sprintf("%s is named as if it held a host resource", name))
				}
			}
			checkType(f.Type, name, seen, problems)
		}
	}
}

// TestDescribe checks that a failure names its node and step as the graph has them,
// and that one whose references the graph doesn't have, as malformed data may, is its message alone.
func TestDescribe(t *testing.T) {
	g := &graph.Graph{Nodes: []graph.Node{{Desc: `task "t"`, Task: &graph.Task{
		Steps: []graph.Step{{Op: graph.OpRun, Display: `run(["false"])`}},
	}}}}
	step := func(i int) *int { return &i }
	cases := []struct {
		name    string
		failure Failure
		want    string
	}{
		{
			name:    "step",
			failure: Failure{Nodes: []graph.Ref{0}, Step: step(0), Message: "exit 1", Hint: "look"},
			want:    `task "t": run(["false"]) failed: exit 1; look`,
		},
		{
			name:    "node",
			failure: Failure{Nodes: []graph.Ref{0}, Message: "cancelled"},
			want:    `task "t": cancelled`,
		},
		{
			name:    "no node",
			failure: Failure{Message: "refused"},
			want:    "refused",
		},
		{
			name:    "negative node",
			failure: Failure{Nodes: []graph.Ref{-1}, Message: "bad"},
			want:    "bad",
		},
		{
			name:    "node past the end",
			failure: Failure{Nodes: []graph.Ref{1}, Message: "bad"},
			want:    "bad",
		},
		{
			name:    "negative step",
			failure: Failure{Nodes: []graph.Ref{0}, Step: step(-1), Message: "bad"},
			want:    `task "t": bad`,
		},
		{
			name:    "step past the end",
			failure: Failure{Nodes: []graph.Ref{0}, Step: step(1), Message: "bad"},
			want:    `task "t": bad`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.failure.Describe(g); got != c.want {
				t.Errorf("Describe = %q, want %q", got, c.want)
			}
		})
	}
}
