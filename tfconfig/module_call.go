// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tfconfig

// ModuleCall represents a "module" block within a module. That is, a
// declaration of a child module from inside its parent.
type ModuleCall struct {
	Name              string `json:"name"`
	Source            string `json:"source"`
	SourceExpression  string `json:"source_expression,omitempty"`
	Version           string `json:"version,omitempty"`
	VersionExpression string `json:"version_expression,omitempty"`

	Pos SourcePos `json:"pos"`
}
