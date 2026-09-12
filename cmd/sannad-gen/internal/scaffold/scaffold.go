// Package scaffold generates a module skeleton against the public SDK.
//
// The generator exists because the rules a correct module follows are not
// discoverable from the SDK's type signatures. A module compiles perfectly while
// importing kernel internals, while carrying a tenant ID in its request, and
// while holding a contract that cannot cross a tier boundary. Each of those is
// forbidden by an ADR and each produces working code that fails later, in a
// different tier or a different tenant's data.
//
// So this generator's job is not to save typing. It is to make the first version
// of a module correct by construction, and to emit the tests that keep it that
// way after the author starts editing.
package scaffold

import (
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"unicode"
)

// Spec is what the author supplies. Everything else is derived, because every
// derived name is one more chance for two files to disagree.
type Spec struct {
	// ModuleID is the reverse-DNS identity, e.g. com.acme.parties. It is the
	// registry key and must be globally unique.
	ModuleID string

	// Capability is the capability this module provides, e.g. parties.reader.
	// The version is appended separately so the ref and the Go name stay in step.
	Capability string

	// Version is the capability version, e.g. v1.
	Version string

	// Dir is where the module is written.
	Dir string
}

// Plan is a Spec with every derived name resolved. Templates read this and never
// compute, so a name appears in exactly one place.
type Plan struct {
	Spec

	// Package is the Go package name, derived from the last ModuleID segment.
	Package string

	// TypePrefix is the exported prefix for generated types, e.g. Parties.
	TypePrefix string

	// CapabilityConst is the exported var holding the CapabilityRef.
	CapabilityConst string

	// Table is the storage table name. Prefixed with the package so two modules
	// cannot collide in a shared database.
	Table string

	// HookRef is the hook point the module offers, so it is extensible from the
	// first commit rather than after someone asks.
	HookRef string
}

var (
	moduleIDPattern   = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9]+)+$`)
	capabilityPattern = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9]+)*$`)
	versionPattern    = regexp.MustCompile(`^v[0-9]+$`)
)

// Validate refuses a spec that would produce a module violating a rule the
// kernel enforces elsewhere. Refusing here is the cheapest place: the author has
// not written anything yet.
func (s Spec) Validate() error {
	if !moduleIDPattern.MatchString(s.ModuleID) {
		return fmt.Errorf("module ID %q must be reverse-DNS, lowercase, e.g. com.acme.parties", s.ModuleID)
	}
	if !capabilityPattern.MatchString(s.Capability) {
		return fmt.Errorf("capability %q must be lowercase dotted, e.g. parties.reader", s.Capability)
	}
	if !versionPattern.MatchString(s.Version) {
		return fmt.Errorf("version %q must be vN, e.g. v1", s.Version)
	}
	if s.Dir == "" {
		return fmt.Errorf("output directory is required")
	}
	return nil
}

// NewPlan derives every name once.
func NewPlan(s Spec) (Plan, error) {
	if err := s.Validate(); err != nil {
		return Plan{}, err
	}

	segments := strings.Split(s.ModuleID, ".")
	pkg := segments[len(segments)-1]
	if token.IsKeyword(pkg) {
		return Plan{}, fmt.Errorf("module ID's last segment %q is a Go keyword; pick another", pkg)
	}

	// The capability's leaf noun drives the type names: parties.reader gives
	// Parties, so the generated types read as the domain rather than the verb.
	capSegments := strings.Split(s.Capability, ".")
	prefix := exportize(capSegments[0])

	return Plan{
		Spec:            s,
		Package:         pkg,
		TypePrefix:      prefix,
		CapabilityConst: "Capability" + exportize(strings.Join(capSegments, ".")),
		Table:           pkg + "_" + strings.ReplaceAll(capSegments[0], ".", "_"),
		HookRef:         capSegments[0] + ".before_write",
	}, nil
}

// exportize turns a dotted lowercase name into an exported Go identifier.
func exportize(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		if r == '.' || r == '_' || r == '-' {
			upper = true
			continue
		}
		if upper {
			b.WriteRune(unicode.ToUpper(r))
			upper = false
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// File is one generated file.
type File struct {
	Name    string
	Content string
}

// Render produces every file for a plan without writing anything, so the caller
// can preview, diff, or test the output without touching a filesystem.
func Render(plan Plan) ([]File, error) {
	sources := []struct {
		name string
		tmpl string
	}{
		{"module.go", moduleTemplate},
		{"contract.go", contractTemplate},
		{"module_test.go", moduleTestTemplate},
		{"boundary_test.go", boundaryTestTemplate},
		{"README.md", readmeTemplate},
		{filepath.Join("migrations", "00001_init.sql"), migrationTemplate},
	}

	files := make([]File, 0, len(sources))
	for _, src := range sources {
		t, err := template.New(src.name).Parse(src.tmpl)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", src.name, err)
		}
		var out strings.Builder
		if err := t.Execute(&out, plan); err != nil {
			return nil, fmt.Errorf("render %s: %w", src.name, err)
		}
		files = append(files, File{Name: src.name, Content: out.String()})
	}
	return files, nil
}

// Write renders a plan to disk.
//
// It refuses to overwrite. A generator that clobbers is a generator nobody runs
// twice, and the second run is exactly when an author wants to compare.
func Write(plan Plan) ([]string, error) {
	files, err := Render(plan)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(plan.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", plan.Dir, err)
	}

	// Check every target before writing any, so a collision does not leave a
	// half-generated module behind.
	paths := make([]string, 0, len(files))
	for _, f := range files {
		path := filepath.Join(plan.Dir, f.Name)
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("%s already exists; refusing to overwrite", path)
		}
		paths = append(paths, path)
	}

	for i, f := range files {
		// A file may sit in a subdirectory — migrations/ does — and MkdirAll on
		// the module root does not create it.
		if dir := filepath.Dir(paths[i]); dir != plan.Dir {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create %s: %w", dir, err)
			}
		}
		if err := os.WriteFile(paths[i], []byte(f.Content), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", paths[i], err)
		}
	}
	return paths, nil
}
