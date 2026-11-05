package provider

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

func TestCommandCodeVersionProbeSuppressesAutoUpdate(t *testing.T) {
	args := VersionProbeArgs(CommandCode)
	if len(args) != 2 || args[0] != "--no-auto-update" || args[1] != "--version" {
		t.Fatalf("VersionProbeArgs(CommandCode) = %v, want --no-auto-update --version", args)
	}
}

func TestEveryProviderHasAVersionProbeOrReason(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "descriptor.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	skipped := map[Name]string{}
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		typ, ok := spec.Type.(*ast.Ident)
		if !ok || typ.Name != "Name" {
			return true
		}
		for _, value := range spec.Values {
			literal, ok := value.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatal("provider names must be string constants")
			}
			name, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			count++
			if len(VersionProbeArgs(Name(name))) == 0 && strings.TrimSpace(skipped[Name(name)]) == "" {
				t.Errorf("provider %s needs a version probe or a reason to skip it", name)
			}
		}
		return false
	})
	if count == 0 {
		t.Fatal("no provider names checked")
	}
}
