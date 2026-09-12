// Command sannad-gen scaffolds a Sannad module against the public SDK.
//
// Usage:
//
//	sannad-gen module -id com.acme.parties -capability parties.reader -out ./parties
//
// The generated module compiles, passes its own tests, and satisfies the rules
// the kernel enforces elsewhere: public SDK only, tenant from context, portable
// contracts, no engine struct tags.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sannados/sannad/cmd/sannad-gen/internal/scaffold"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sannad-gen:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "module" {
		usage()
		return fmt.Errorf("expected subcommand: module")
	}

	fs := flag.NewFlagSet("module", flag.ContinueOnError)
	spec := scaffold.Spec{}
	fs.StringVar(&spec.ModuleID, "id", "", "reverse-DNS module ID, e.g. com.acme.parties")
	fs.StringVar(&spec.Capability, "capability", "", "capability name, e.g. parties.reader")
	fs.StringVar(&spec.Version, "version", "v1", "capability version")
	fs.StringVar(&spec.Dir, "out", "", "output directory")
	dryRun := fs.Bool("dry-run", false, "print what would be written without writing it")

	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if spec.ModuleID == "" || spec.Capability == "" {
		fs.Usage()
		return fmt.Errorf("-id and -capability are required")
	}
	if spec.Dir == "" {
		spec.Dir = filepath.Base(spec.ModuleID)
	}

	plan, err := scaffold.NewPlan(spec)
	if err != nil {
		return err
	}

	if *dryRun {
		files, err := scaffold.Render(plan)
		if err != nil {
			return err
		}
		for _, f := range files {
			fmt.Printf("=== %s ===\n%s\n", filepath.Join(plan.Dir, f.Name), f.Content)
		}
		return nil
	}

	paths, err := scaffold.Write(plan)
	if err != nil {
		return err
	}
	for _, p := range paths {
		fmt.Println("created", p)
	}
	fmt.Printf("\nNext: add a migration for table %q, then run go test ./...\n", plan.Table)
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `sannad-gen scaffolds a Sannad module.

  sannad-gen module -id com.acme.parties -capability parties.reader [-version v1] [-out DIR] [-dry-run]`)
}
