package database

// ObjectPageRequest selects a bounded catalog page; Kind uses the driver's vocabulary.
type ObjectPageRequest struct {
	PageRequest
	Kind string
}

// ObjectRequest identifies one object without accepting SQL fragments.
// IdentityArguments is used only by drivers whose routines can be overloaded.
type ObjectRequest struct {
	Kind, Schema, Name string
	IdentityArguments  *string
}

// Definition is a server-provided definition, never evaluated by metadata tools.
type Definition struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Definition string `json:"definition"`
}
