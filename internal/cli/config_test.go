package cli

import (
	"testing"
)

// TestLoadMistakes checks the errors for load() statements and the .star files they name.
func TestLoadMistakes(t *testing.T) {
	tests := []struct {
		name  string
		build string
		files map[string]string
		want  []string
	}{
		{
			name:  "relative label",
			build: `load(":defs.star", "x")`,
			want:  []string{"/project/BUILD:1:1:", `a .star file is named by its label, which starts with "//"`},
		},
		{
			name:  "not a .star file",
			build: `load("//:BUILD", "x")`,
			want:  []string{"/project/BUILD:1:1:", `such as "//:defs.star"`},
		},
		{
			name:  "missing file",
			build: `load("//:defs.star", "x")`,
			want:  []string{"/project/BUILD:1:1:", "cannot load //:defs.star: no file /project/defs.star"},
		},
		{
			name:  "missing name",
			build: `load("//:defs.star", "nope")`,
			files: map[string]string{"defs.star": "X = 1\n"},
			want:  []string{"/project/BUILD:1:1:", "load: name nope not found in module //:defs.star"},
		},
		{
			name:  "error in the file",
			build: `load("//:defs.star", "X")`,
			files: map[string]string{"defs.star": "X = 1\nY = X + \"a\"\n"},
			want:  []string{"cannot load //:defs.star: /project/defs.star:2:7: unknown binary op: int + string"},
		},
		{
			name:  "declaring as it is loaded",
			build: `load("//:defs.star", "X")`,
			files: map[string]string{"defs.star": `X = 1` + "\n" + `glob(name="code", include=["app.sh"])`},
			want: []string{
				"/project/defs.star:2:5:",
				"glob: a .star file can't declare a target as it is loaded;",
				"declare it in a function that a BUILD file calls",
			},
		},
		{
			name:  "relative label as it is loaded",
			build: `load("//:defs.star", "X")`,
			files: map[string]string{"defs.star": `X = dep("base")`},
			want: []string{
				"/project/defs.star:1:8:",
				`dep: label "base": as a .star file is loaded, labels must start with "//"`,
			},
		},
		{
			name: "argument checked once used",
			build: `load("//:defs.star", "BASE")
layer(name="base", params={"mode": "release"}, base="scratch")
layer(name="app", base=BASE)`,
			files: map[string]string{"defs.star": `BASE = dep("//:base", mod="debug")`},
			want: []string{
				"/project/BUILD:3:6:",
				`layer: base: "//:base" has no parameter "mod"; did you mean "mode"?`,
			},
		},
		{
			name:  "cycle of loads",
			build: `load("//:a.star", "A")`,
			files: map[string]string{
				"a.star": `load("//:b.star", "B")` + "\nA = 1\n",
				"b.star": `load("//:a.star", "A")` + "\nB = 1\n",
			},
			want: []string{"//:a.star loads itself, through the files it loads"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"BUILD": tt.build, "app.sh": ""}
			for name, content := range tt.files {
				files[name] = content
			}
			vamos(t, project(t, files), "build", "app", "-C", projectDir).failsWith(t, false, tt.want...)
		})
	}
}
