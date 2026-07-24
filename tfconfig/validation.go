package tfconfig

// Validation represents a validation block attached to a variable.
type Validation struct {
	Condition    string    `json:"condition"`
	ErrorMessage string    `json:"error_message"`
	Pos          SourcePos `json:"pos"`
}
