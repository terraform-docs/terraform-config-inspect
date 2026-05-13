// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tfconfig

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

func loadModule(fs FS, dir string) (*Module, Diagnostics) {
	mod := NewModule(dir)
	primaryPaths, diags := dirFiles(fs, dir)

	parser := hclparse.NewParser()

	files := make([]*hcl.File, 0, len(primaryPaths))
	for _, filename := range primaryPaths {
		var file *hcl.File
		var fileDiags hcl.Diagnostics

		b, err := fs.ReadFile(filename)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Failed to read file",
				Detail:   fmt.Sprintf("The configuration file %q could not be read.", filename),
			})
			continue
		}
		if strings.HasSuffix(filename, ".json") {
			file, fileDiags = parser.ParseJSON(b, filename)
		} else {
			file, fileDiags = parser.ParseHCL(b, filename)
		}
		diags = append(diags, fileDiags...)
		if file == nil {
			continue
		}
		files = append(files, file)
	}

	// First pass: collect variable defaults and locals so that module
	// `source` and `version` arguments using them (a Terraform 1.15+
	// feature) can be resolved.
	evalCtx := buildEvalContext(files)

	for _, file := range files {
		contentDiags := LoadModuleFromFileWithContext(file, mod, evalCtx)
		diags = append(diags, contentDiags...)
	}

	return mod, diagnosticsHCL(diags)
}

// LoadModuleFromFile reads given file, interprets it and stores in given Module
// This is useful for any caller which does tokenization/parsing on its own
// e.g. because it will reuse these parsed files later for more detailed
// interpretation.
func LoadModuleFromFile(file *hcl.File, mod *Module) hcl.Diagnostics {
	return LoadModuleFromFileWithContext(file, mod, nil)
}

// LoadModuleFromFileWithContext is like LoadModuleFromFile, but accepts an
// optional hcl.EvalContext that is used to resolve variable and local
// references in module `source` and `version` arguments (a Terraform 1.15+
// feature). If evalCtx is nil, the behavior is equivalent to
// LoadModuleFromFile.
func LoadModuleFromFileWithContext(file *hcl.File, mod *Module, evalCtx *hcl.EvalContext) hcl.Diagnostics {
	var diags hcl.Diagnostics
	content, _, contentDiags := file.Body.PartialContent(rootSchema)
	diags = append(diags, contentDiags...)

	for _, block := range content.Blocks {
		switch block.Type {

		case "terraform":
			content, _, contentDiags := block.Body.PartialContent(terraformBlockSchema)
			diags = append(diags, contentDiags...)

			if attr, defined := content.Attributes["required_version"]; defined {
				var version string
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &version)
				diags = append(diags, valDiags...)
				if !valDiags.HasErrors() {
					mod.RequiredCore = append(mod.RequiredCore, version)
				}
			}

			for _, innerBlock := range content.Blocks {
				switch innerBlock.Type {
				case "required_providers":
					reqs, reqsDiags := decodeRequiredProvidersBlock(innerBlock)
					diags = append(diags, reqsDiags...)
					for name, req := range reqs {
						if _, exists := mod.RequiredProviders[name]; !exists {
							mod.RequiredProviders[name] = req
						} else {
							if req.Source != "" {
								source := mod.RequiredProviders[name].Source
								if source != "" && source != req.Source {
									diags = append(diags, &hcl.Diagnostic{
										Severity: hcl.DiagError,
										Summary:  "Multiple provider source attributes",
										Detail:   fmt.Sprintf("Found multiple source attributes for provider %s: %q, %q", name, source, req.Source),
										Subject:  &innerBlock.DefRange,
									})
								} else {
									mod.RequiredProviders[name].Source = req.Source
								}
							}

							mod.RequiredProviders[name].VersionConstraints = append(mod.RequiredProviders[name].VersionConstraints, req.VersionConstraints...)
						}
					}
				}
			}

		case "variable":
			content, _, contentDiags := block.Body.PartialContent(variableSchema)
			diags = append(diags, contentDiags...)

			name := block.Labels[0]
			v := &Variable{
				Name: name,
				Pos:  sourcePosHCL(block.DefRange),
			}

			mod.Variables[name] = v

			if attr, defined := content.Attributes["type"]; defined {
				// We handle this particular attribute in a somewhat-tricky way:
				// since Terraform may evolve its type expression syntax in
				// future versions, we don't want to be overly-strict in how
				// we handle it here, and so we'll instead just take the raw
				// source provided by the user, using the source location
				// information in the expression object.
				//
				// However, older versions of Terraform expected the type
				// to be a string containing a keyword, so we'll need to
				// handle that as a special case first for backward compatibility.

				var typeExpr string

				var typeExprAsStr string
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &typeExprAsStr)
				if !valDiags.HasErrors() {
					typeExpr = typeExprAsStr
				} else {
					rng := attr.Expr.Range()
					typeExpr = string(rng.SliceBytes(file.Bytes))
				}

				v.Type = typeExpr
			}

			if attr, defined := content.Attributes["description"]; defined {
				var description string
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &description)
				diags = append(diags, valDiags...)
				v.Description = description
			}

			if attr, defined := content.Attributes["default"]; defined {
				// To avoid the caller needing to deal with cty here, we'll
				// use its JSON encoding to convert into an
				// approximately-equivalent plain Go interface{} value
				// to return.
				val, valDiags := attr.Expr.Value(nil)
				diags = append(diags, valDiags...)
				if val.IsWhollyKnown() { // should only be false if there are errors in the input
					valJSON, err := ctyjson.Marshal(val, val.Type())
					if err != nil {
						// Should never happen, since all possible known
						// values have a JSON mapping.
						panic(fmt.Errorf("failed to serialize default value as JSON: %s", err))
					}
					var def interface{}
					err = json.Unmarshal(valJSON, &def)
					if err != nil {
						// Again should never happen, because valJSON is
						// guaranteed valid by ctyjson.Marshal.
						panic(fmt.Errorf("failed to re-parse default value from JSON: %s", err))
					}
					v.Default = def
				}
			} else {
				v.Required = true
			}

			if attr, defined := content.Attributes["sensitive"]; defined {
				var sensitive bool
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &sensitive)
				diags = append(diags, valDiags...)
				v.Sensitive = sensitive
			}

		case "output":

			content, _, contentDiags := block.Body.PartialContent(outputSchema)
			diags = append(diags, contentDiags...)

			name := block.Labels[0]
			o := &Output{
				Name: name,
				Pos:  sourcePosHCL(block.DefRange),
			}

			mod.Outputs[name] = o

			if attr, defined := content.Attributes["description"]; defined {
				var description string
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &description)
				diags = append(diags, valDiags...)
				o.Description = description
			}

			if attr, defined := content.Attributes["sensitive"]; defined {
				var sensitive bool
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &sensitive)
				diags = append(diags, valDiags...)
				o.Sensitive = sensitive
			}

		case "provider":

			content, _, contentDiags := block.Body.PartialContent(providerConfigSchema)
			diags = append(diags, contentDiags...)

			name := block.Labels[0]
			// Even if there isn't an explicit version required, we still
			// need an entry in our map to signal the unversioned dependency.
			if _, exists := mod.RequiredProviders[name]; !exists {
				mod.RequiredProviders[name] = &ProviderRequirement{}
			}
			if attr, defined := content.Attributes["version"]; defined {
				var version string
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &version)
				diags = append(diags, valDiags...)
				if !valDiags.HasErrors() {
					mod.RequiredProviders[name].VersionConstraints = append(mod.RequiredProviders[name].VersionConstraints, version)
				}
			}

			providerKey := name
			var alias string
			if attr, defined := content.Attributes["alias"]; defined {
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &alias)
				diags = append(diags, valDiags...)
				if !valDiags.HasErrors() && alias != "" {
					providerKey = fmt.Sprintf("%s.%s", name, alias)
				}
			}

			mod.ProviderConfigs[providerKey] = &ProviderConfig{
				Name:  name,
				Alias: alias,
			}

		case "resource", "data":

			content, _, contentDiags := block.Body.PartialContent(resourceSchema)
			diags = append(diags, contentDiags...)

			typeName := block.Labels[0]
			name := block.Labels[1]

			r := &Resource{
				Type: typeName,
				Name: name,
				Pos:  sourcePosHCL(block.DefRange),
			}

			var resourcesMap map[string]*Resource

			switch block.Type {
			case "resource":
				r.Mode = ManagedResourceMode
				resourcesMap = mod.ManagedResources
			case "data":
				r.Mode = DataResourceMode
				resourcesMap = mod.DataResources
			}

			key := r.MapKey()

			resourcesMap[key] = r

			if attr, defined := content.Attributes["provider"]; defined {
				// New style here is to provide this as a naked traversal
				// expression, but we also support quoted references for
				// older configurations that predated this convention.
				traversal, travDiags := hcl.AbsTraversalForExpr(attr.Expr)
				if travDiags.HasErrors() {
					traversal = nil // in case we got any partial results

					// Fall back on trying to parse as a string
					var travStr string
					valDiags := gohcl.DecodeExpression(attr.Expr, nil, &travStr)
					if !valDiags.HasErrors() {
						var strDiags hcl.Diagnostics
						traversal, strDiags = hclsyntax.ParseTraversalAbs([]byte(travStr), "", hcl.Pos{})
						if strDiags.HasErrors() {
							traversal = nil
						}
					}
				}

				// If we get out here with a nil traversal then we didn't
				// succeed in processing the input.
				if len(traversal) > 0 {
					providerName := traversal.RootName()
					alias := ""
					if len(traversal) > 1 {
						if getAttr, ok := traversal[1].(hcl.TraverseAttr); ok {
							alias = getAttr.Name
						}
					}
					r.Provider = ProviderRef{
						Name:  providerName,
						Alias: alias,
					}
				} else {
					diags = append(diags, &hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "Invalid provider reference",
						Detail:   "Provider argument requires a provider name followed by an optional alias, like \"aws.foo\".",
						Subject:  attr.Expr.Range().Ptr(),
					})
				}
			} else {
				// If provider _isn't_ set then we'll infer it from the
				// resource type.
				r.Provider = ProviderRef{
					Name: resourceTypeDefaultProviderName(r.Type),
				}
			}

		case "module":

			content, _, contentDiags := block.Body.PartialContent(moduleCallSchema)
			diags = append(diags, contentDiags...)

			name := block.Labels[0]
			mc := &ModuleCall{
				Name: block.Labels[0],
				Pos:  sourcePosHCL(block.DefRange),
			}

			// check if this is overriding an existing module
			var origSource string
			if origMod, exists := mod.ModuleCalls[name]; exists {
				origSource = origMod.Source
			}

			mod.ModuleCalls[name] = mc

			if attr, defined := content.Attributes["source"]; defined {
				var source string
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &source)
				if !valDiags.HasErrors() {
					mc.Source = source
				} else if isReferenceExpr(attr.Expr) {
					// Terraform 1.15+ allows variables/locals here.
					if v, ok := tryEvalString(attr.Expr, evalCtx); ok {
						mc.Source = v
					} else {
						rng := attr.Expr.Range()
						mc.SourceExpr = string(rng.SliceBytes(file.Bytes))
					}
				} else {
					diags = append(diags, valDiags...)
				}
			}

			if mc.Source == "" && mc.SourceExpr == "" {
				mc.Source = origSource
			}

			if attr, defined := content.Attributes["version"]; defined {
				var version string
				valDiags := gohcl.DecodeExpression(attr.Expr, nil, &version)
				if !valDiags.HasErrors() {
					mc.Version = version
				} else if isReferenceExpr(attr.Expr) {
					// Terraform 1.15+ allows variables/locals here.
					if v, ok := tryEvalString(attr.Expr, evalCtx); ok {
						mc.Version = v
					} else {
						rng := attr.Expr.Range()
						mc.VersionExpr = string(rng.SliceBytes(file.Bytes))
					}
				} else {
					diags = append(diags, valDiags...)
				}
			}

		case "locals":

		default:
			// Should never happen because our cases above should be
			// exhaustive for our schema.
			panic(fmt.Errorf("unhandled block type %q", block.Type))
		}
	}

	return diags
}

// buildEvalContext walks the parsed files and constructs an hcl.EvalContext
// containing variable defaults under `var` and resolved local values under
// `local`. Locals that depend on other locals are resolved iteratively.
//
// This supports the Terraform 1.15+ feature that allows the `source` and
// `version` arguments of a `module` block to reference variables and locals.
func buildEvalContext(files []*hcl.File) *hcl.EvalContext {
	vars := map[string]cty.Value{}
	type pendingLocal struct {
		name string
		expr hcl.Expression
	}
	var locals []pendingLocal

	for _, file := range files {
		content, _, _ := file.Body.PartialContent(rootSchema)
		for _, block := range content.Blocks {
			switch block.Type {
			case "variable":
				if len(block.Labels) == 0 {
					continue
				}
				name := block.Labels[0]
				varContent, _, _ := block.Body.PartialContent(variableSchema)
				attr, defined := varContent.Attributes["default"]
				if !defined {
					continue
				}
				val, valDiags := attr.Expr.Value(nil)
				if valDiags.HasErrors() || !val.IsWhollyKnown() {
					continue
				}
				vars[name] = val
			case "locals":
				attrs, _ := block.Body.JustAttributes()
				for name, attr := range attrs {
					locals = append(locals, pendingLocal{name: name, expr: attr.Expr})
				}
			}
		}
	}

	resolved := map[string]cty.Value{}

	mkCtx := func() *hcl.EvalContext {
		ctx := &hcl.EvalContext{Variables: map[string]cty.Value{}}
		if len(vars) > 0 {
			ctx.Variables["var"] = cty.ObjectVal(vars)
		}
		if len(resolved) > 0 {
			ctx.Variables["local"] = cty.ObjectVal(resolved)
		}
		return ctx
	}

	// Iteratively resolve locals so that locals depending on other locals
	// can be evaluated. Bound to a small number of passes to avoid
	// excessive cycle depth.
	for i := 0; i < 5; i++ {
		progressed := false
		for _, p := range locals {
			if _, done := resolved[p.name]; done {
				continue
			}
			v, d := p.expr.Value(mkCtx())
			if d.HasErrors() || !v.IsWhollyKnown() {
				continue
			}
			resolved[p.name] = v
			progressed = true
		}
		if !progressed {
			break
		}
	}

	return mkCtx()
}

// isReferenceExpr reports whether expr contains any variable traversals
// (e.g. `var.foo`, `local.bar`). Pure literal expressions return false.
func isReferenceExpr(expr hcl.Expression) bool {
	return len(expr.Variables()) > 0
}

// tryEvalString attempts to evaluate expr against evalCtx and convert the
// result to a string. Returns ("", false) if evalCtx is nil, the expression
// has unresolved references, or the result is not convertible to a string.
func tryEvalString(expr hcl.Expression, evalCtx *hcl.EvalContext) (string, bool) {
	if evalCtx == nil {
		return "", false
	}
	v, d := expr.Value(evalCtx)
	if d.HasErrors() || v.IsNull() || !v.IsKnown() {
		return "", false
	}
	if v.Type() != cty.String {
		conv, err := convert.Convert(v, cty.String)
		if err != nil {
			return "", false
		}
		v = conv
	}
	return v.AsString(), true
}
