package command

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/importer"
)

// Defaults of an import.
const (
	importWorkers = 4
	fetchTimeout  = 2 * time.Minute
)

// runImport stores thumbnails made elsewhere as the masters of the videos a
// manifest names, by path inside one space. The rows that do not go through
// are printed as the run comes to them, the counts at the end.
func runImport(ctx context.Context, version string, args []string) error {
	flags := newFlags("import")
	space := flags.String("space", "", "Id or exact name of the space the paths of the manifest are in. Required.")
	manifest := flags.String("manifest", "", "CSV with a header naming the columns path and thumb: the path of the video inside the space and the URL of its thumbnail. Required.")
	workers := flags.Int("workers", importWorkers, "How many thumbnails are fetched at once.")
	dryRun := flags.Bool("dry-run", false, "Look the files up and fetch nothing.")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *space == "" || *manifest == "" {
		return errors.New("import needs -space and -manifest")
	}

	file, err := os.Open(*manifest)
	if err != nil {
		return err
	}
	defer file.Close()
	rows, err := importer.OpenManifest(file)
	if err != nil {
		return err
	}

	t, err := openTools(ctx, version, false)
	if err != nil {
		return err
	}
	defer t.Close()

	opts := importer.Options{
		Space:      *space,
		MasterSize: t.cfg.MasterSize,
		Workers:    *workers,
		DryRun:     *dryRun,
		NotImported: func(path, reason string) {
			fmt.Fprintf(os.Stdout, "%s: %s\n", path, reason)
		},
	}
	report, err := importer.Run(ctx, t.gateway, t.thumbs, &http.Client{Timeout: fetchTimeout}, t.video, rows.Rows(), opts, t.log)
	if report != nil {
		printImport(report, *dryRun)
	}
	if err != nil {
		return err
	}
	if !report.OK() {
		return errors.New("import is incomplete: see the rows listed above")
	}
	return nil
}

func printImport(report *importer.Report, dryRun bool) {
	verb := "imported"
	if dryRun {
		verb = "to import"
	}
	fmt.Fprintf(os.Stdout, "rows %d, %s %d, skipped %d, not found %d, not a video %d, failed %d\n", //nolint:gosec // G705: the stdout of a CLI, not a page
		report.Rows, verb, report.Imported, report.Skipped, report.NotFound, report.NotVideo, report.Failed)
}
