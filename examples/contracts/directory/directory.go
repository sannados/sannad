// Package directory is a capability contract: the types two modules agree on,
// and the reference they meet at. It contains no implementation.
//
// This package exists to solve a problem the rest of the examples quietly have.
// A capability is meant to decouple caller from provider — the caller names a
// reference, and whoever registered it answers. But if the request and response
// types live in the provider's package, the caller has to import the provider to
// name them, and the decoupling is fake: there is still a compile-time
// dependency on the implementation, the provider can never be replaced, and it
// can never move to WASM or an external app.
//
// So the contract lives here, and both sides import it:
//
//	provider  ->  imports directory, registers directory.Reader
//	consumer  ->  imports directory, calls directory.Reader
//	neither imports the other
//
// The entrypoint is different, and should import both: wiring modules together
// is exactly its job. The indirection is for module-to-module calls, not for
// entrypoint-to-module wiring.
//
// A contract package must stay implementation-free. The moment it imports a
// provider, every consumer inherits that dependency and the boundary is gone.
package directory

import "github.com/sannados/sannad/pkg/modulekit"

// Reader is the capability reference. It lives with the types rather than with
// the provider, because a caller needs the reference and the types together and
// should not import an implementation to get either.
var Reader = modulekit.CapabilityRef{Name: "directory.people.reader", Version: "v1"}

// Person is one directory entry.
//
// Carries no tenant field: the tenant travels in the context and is resolved
// from the caller's credentials. A tenant in a payload is a value the caller
// controls, which is the shape of a cross-tenant read.
type Person struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// ListRequest asks for directory entries.
type ListRequest struct {
	// Limit caps the result set. Zero means the provider's default.
	Limit int `json:"limit"`
}

// ListResponse carries the result.
type ListResponse struct {
	People []Person `json:"people"`
}
