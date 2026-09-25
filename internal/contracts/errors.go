package contracts

// Code identifies an expected safe application failure.
type Code string

const (
	ConfigInvalid      Code = "CONFIG_INVALID"
	ConnectionNotFound Code = "CONNECTION_NOT_FOUND"
	CredentialMissing  Code = "CREDENTIAL_MISSING"
	VaultUnavailable   Code = "VAULT_UNAVAILABLE"
	ConnectFailed      Code = "CONNECT_FAILED"
	ScopeDenied        Code = "SCOPE_DENIED"
	QueryUnsupported   Code = "QUERY_UNSUPPORTED"
	QueryTimeout       Code = "QUERY_TIMEOUT"
	ResourceLimit      Code = "RESOURCE_LIMIT"
	ReadOnlyViolation  Code = "READ_ONLY_VIOLATION"
	PermissionDenied   Code = "PERMISSION_DENIED"
	QueryFailed        Code = "QUERY_FAILED"
	StaleCursor        Code = "STALE_CURSOR"
	InvalidArgument    Code = "INVALID_ARGUMENT"
	ServiceUnavailable Code = "SERVICE_UNAVAILABLE"
	Cancelled          Code = "CANCELLED"
)

// Failure is a redacted error object shared by future CLI and MCP operations.
type Failure struct {
	Code      Code   `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	SQLState  string `json:"sqlstate,omitempty"`
}
