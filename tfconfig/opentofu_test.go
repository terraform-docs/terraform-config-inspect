package tfconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOpenTofuMetadata(t *testing.T) {
	dir := t.TempDir()
	config := `
variable "token" {
  type      = string
  ephemeral = true

  validation {
    condition     = length(var.token) > 8
    error_message = "token must be longer than eight characters"
  }
}

output "token" {
  value     = var.token
  ephemeral = true
}

module "child" {
  source  = local.module_source
  version = var.module_version
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.tofu"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	module, diags := LoadModule(dir)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Error())
	}

	input := module.Variables["token"]
	if !input.Ephemeral {
		t.Error("expected variable to be ephemeral")
	}
	if len(input.Validations) != 1 {
		t.Fatalf("expected one validation, got %d", len(input.Validations))
	}
	if got, want := input.Validations[0].Condition, "length(var.token) > 8"; got != want {
		t.Errorf("condition = %q, want %q", got, want)
	}
	if got, want := input.Validations[0].ErrorMessage, `"token must be longer than eight characters"`; got != want {
		t.Errorf("error message = %q, want %q", got, want)
	}
	if !module.Outputs["token"].Ephemeral {
		t.Error("expected output to be ephemeral")
	}
	call := module.ModuleCalls["child"]
	if got, want := call.SourceExpression, "local.module_source"; got != want {
		t.Errorf("source expression = %q, want %q", got, want)
	}
	if got, want := call.VersionExpression, "var.module_version"; got != want {
		t.Errorf("version expression = %q, want %q", got, want)
	}
}
