// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tfconfig

// Output represents a single output from a Terraform module.
type Output struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Sensitive   bool      `json:"sensitive,omitempty"`
	Ephemeral   bool      `json:"ephemeral,omitempty"`
	Pos         SourcePos `json:"pos"`
}
