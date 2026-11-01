package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/clidive410/membership/internal/config"
	"github.com/clidive410/membership/internal/documents"
	"github.com/clidive410/membership/internal/legacy"
)

func documentsCmd(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "import-legacy" {
		return errors.New("usage: membership documents import-legacy -catalog-dsn DSN -files-dir DIR [-dry-run] [-v]")
	}
	fs := flag.NewFlagSet("documents import-legacy", flag.ContinueOnError)
	catalogDSN := fs.String("catalog-dsn", "", "DSN of a database with the WordPress tables dbt_wpfb_cats and dbt_wpfb_files")
	filesDir := fs.String("files-dir", "", "directory of the legacy files (the old wp-content/uploads/filebase/)")
	dryRun := fs.Bool("dry-run", false, "check everything, then roll back and copy nothing")
	verbose := fs.Bool("v", false, "list the legacy file ids that are missing or differ from the catalogue")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *catalogDSN == "" || *filesDir == "" {
		return errors.New("-catalog-dsn and -files-dir are required")
	}
	if st, err := os.Stat(*filesDir); err != nil || !st.IsDir() {
		return fmt.Errorf("-files-dir %q is not a directory", *filesDir)
	}
	dir, err := config.LoadDocumentsDir(os.Getenv)
	if err != nil {
		return err
	}
	log := cliLogger()
	mstore, _, closeDB, err := openAppDB(ctx, log)
	if err != nil {
		return err
	}
	defer closeDB()
	catalog, err := legacy.OpenLegacy(ctx, *catalogDSN)
	if err != nil {
		return err
	}
	defer catalog.Close()
	cats, files, err := documents.ReadLegacy(ctx, catalog)
	if err != nil {
		return err
	}
	im := &documents.Importer{DB: mstore.DB, Files: documents.Files{Dir: dir}, FilesDir: *filesDir, DryRun: *dryRun, Log: log}
	res, err := im.Run(ctx, cats, files)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "legacy catalogue: %d categories, %d files\n", len(cats), len(files))
	fmt.Fprintf(out, "categories created: %d, updated: %d\n", res.CategoriesCreated, res.CategoriesUpdated)
	fmt.Fprintf(out, "documents created:  %d, updated: %d\n", res.DocumentsCreated, res.DocumentsUpdated)
	fmt.Fprintf(out, "files copied:       %d (%d bytes) into %s\n", res.FilesCopied, res.BytesCopied, dir)
	fmt.Fprintf(out, "missing files:      %d (archived, noted in the description)\n", len(res.Missing))
	fmt.Fprintf(out, "differ from catalogue (size or md5): %d\n", len(res.Mismatched))
	for _, a := range append(slices.Clone(documents.Levels), documents.Inherit) {
		if n := res.ByAccess[a]; n > 0 {
			fmt.Fprintf(out, "effective access %-11s %d\n", string(a)+":", n)
		}
	}
	if *verbose {
		fmt.Fprintf(out, "missing legacy file ids: %v\nmismatched legacy file ids: %v\n", res.Missing, res.Mismatched)
	}
	if *dryRun {
		fmt.Fprintln(out, "dry run: no changes were committed and no files were copied")
	}
	return nil
}
