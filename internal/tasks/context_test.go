package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validContextResult(root string) map[string]any {
	return map[string]any{
		"schemaVersion": 5, "start": root, "projectBoundary": root,
		"storePath":          filepath.Join(root, ".tasks", "tasks.sqlite"),
		"storeOwnerBoundary": "", "storeSchemaVersion": 0, "engineSchemaVersion": 6,
		"docsPath":        ".tasks/docs/PRD.md",
		"preferencesPath": ".tasks/preferences.json",
		"initialized":     false, "writable": true,
		"blocked": false, "blockedReason": "",
	}
}

func writeContextResponse(t *testing.T, response map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "noise")
	body := "#!/bin/sh\n" +
		"test \"$1 $2 $3\" = \"tasks context --json\" || exit 91\n" +
		"test \"$NOISE_PROJECT_DIR\" = \"$PWD\" || exit 92\n" +
		"printf '%s\\n' '" + strings.ReplaceAll(string(raw), "'", "'\\''") + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEngineContextRequiresExactSchemaAndRegisteredBoundary(t *testing.T) {
	root := t.TempDir()

	t.Run("valid", func(t *testing.T) {
		got, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, validContextResult(root)), root)
		if err != nil || got.DocsPath != ".tasks/docs/PRD.md" {
			t.Fatalf("context = %+v, %v", got, err)
		}
	})

	t.Run("legacy schema", func(t *testing.T) {
		response := validContextResult(root)
		response["schemaVersion"] = 3
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); err == nil {
			t.Fatal("legacy schema accepted")
		}
	})

	t.Run("future schema", func(t *testing.T) {
		response := validContextResult(root)
		response["schemaVersion"] = 6
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); err == nil {
			t.Fatal("future schema accepted")
		}
	})

	t.Run("foreign store owner", func(t *testing.T) {
		response := validContextResult(root)
		response["storeOwnerBoundary"] = t.TempDir()
		_, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root)
		if err == nil || !errors.Is(err, ErrUnsafeTree) {
			t.Fatalf("foreign store owner = %v", err)
		}
	})

	t.Run("engine older than store", func(t *testing.T) {
		response := validContextResult(root)
		response["storeSchemaVersion"] = 7
		response["engineSchemaVersion"] = 6
		_, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root)
		if err == nil || !strings.Contains(err.Error(), "newer than the pinned engine") {
			t.Fatalf("skewed store = %v", err)
		}
	})

	t.Run("schema-2 shape", func(t *testing.T) {
		response := validContextResult(root)
		response["layout"] = "none"
		response["live"] = map[string]any{"present": false, "storePath": ""}
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); err == nil {
			t.Fatal("schema-2 key set accepted")
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		response := validContextResult(root)
		response["surprise"] = true
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); err == nil {
			t.Fatal("unknown field accepted")
		}
	})

	t.Run("missing store path", func(t *testing.T) {
		response := validContextResult(root)
		delete(response, "storePath")
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); err == nil {
			t.Fatal("missing storePath accepted")
		}
	})

	t.Run("unsafe store path", func(t *testing.T) {
		response := validContextResult(root)
		response["storePath"] = "relative/tasks.sqlite"
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); err == nil || errors.Is(err, ErrTaskStateBlocked) {
			t.Fatalf("unsafe store path = %v, want unsupported-contract refusal", err)
		}
	})

	t.Run("unclean store path", func(t *testing.T) {
		response := validContextResult(root)
		response["storePath"] = root + "/.tasks/../.tasks/tasks.sqlite"
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); err == nil || errors.Is(err, ErrTaskStateBlocked) {
			t.Fatalf("unclean store path = %v, want unsupported-contract refusal", err)
		}
	})

	t.Run("absolute docs path", func(t *testing.T) {
		response := validContextResult(root)
		response["docsPath"] = "/etc/PRD.md"
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); err == nil || errors.Is(err, ErrTaskStateBlocked) {
			t.Fatalf("absolute docs path = %v, want unsupported-contract refusal", err)
		}
	})

	t.Run("escaping preferences path", func(t *testing.T) {
		response := validContextResult(root)
		response["preferencesPath"] = "../preferences.json"
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); err == nil || errors.Is(err, ErrTaskStateBlocked) {
			t.Fatalf("escaping preferences path = %v, want unsupported-contract refusal", err)
		}
	})

	t.Run("retired blocked reason", func(t *testing.T) {
		response := validContextResult(root)
		response["blocked"] = true
		response["blockedReason"] = "migration-unsettled"
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); err == nil || errors.Is(err, ErrTaskStateBlocked) {
			t.Fatalf("retired blocked reason = %v, want unsupported-contract refusal", err)
		}
	})

	t.Run("adoption recovery", func(t *testing.T) {
		response := validContextResult(root)
		response["blocked"] = true
		response["blockedReason"] = "adoption-recovery-required"
		response["writable"] = false
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); !errors.Is(err, ErrTaskStateBlocked) {
			t.Fatalf("adoption recovery context = %v, want ErrTaskStateBlocked", err)
		}
	})

	t.Run("current initialized store", func(t *testing.T) {
		response := validContextResult(root)
		response["storeSchemaVersion"] = 6
		response["initialized"] = true
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); err != nil {
			t.Fatalf("current store schema rejected: %v", err)
		}
	})

	t.Run("wrong boundary", func(t *testing.T) {
		response := validContextResult(root)
		response["projectBoundary"] = filepath.Dir(root)
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); !errors.Is(err, ErrUnsafeTree) {
			t.Fatalf("wrong boundary = %v, want ErrUnsafeTree", err)
		}
	})

	t.Run("blocked", func(t *testing.T) {
		response := validContextResult(root)
		response["blocked"] = true
		response["blockedReason"] = "migration-recovery-required"
		if _, err := resolveEngineProjectContext(context.Background(), writeContextResponse(t, response), root); !errors.Is(err, ErrTaskStateBlocked) {
			t.Fatalf("blocked context = %v, want ErrTaskStateBlocked", err)
		}
	})
}
