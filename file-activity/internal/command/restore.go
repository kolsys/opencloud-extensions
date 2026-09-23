package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	commonconfig "github.com/kolsys/opencloud-extensions/common/config"
	"github.com/kolsys/opencloud-extensions/common/s3store"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/restore"
)

// defaultWorkers is how many files a restore moves at once.
const defaultWorkers = 4

// restoreConfig is what a restore needs besides the tree: the bucket the
// platform keeps its blobs in, and the destination.
type restoreConfig struct {
	BlobsEndpoint  string              `json:"blobs_endpoint" env:"RESTORE_BLOBS_S3_ENDPOINT" desc:"URL of the S3 endpoint of the platform. Defaults to the one of the tree."`
	BlobsRegion    string              `json:"blobs_region" env:"RESTORE_BLOBS_S3_REGION" desc:"Region of the bucket of the platform. Defaults to the one of the tree."`
	BlobsBucket    string              `json:"blobs_bucket" env:"RESTORE_BLOBS_S3_BUCKET,required" desc:"Bucket the platform keeps its blobs in."`
	BlobsAccessKey string              `json:"blobs_access_key" env:"RESTORE_BLOBS_S3_ACCESS_KEY" desc:"Access key of the bucket of the platform. Defaults to the one of the tree."`
	BlobsSecretKey commonconfig.Secret `json:"blobs_secret_key" env:"RESTORE_BLOBS_S3_SECRET_KEY" desc:"Secret key of the bucket of the platform. Defaults to the one of the tree."`

	DestUser     string              `json:"dest_user" env:"RESTORE_DEST_USER" desc:"User the files are written as, for Basic authentication."`
	DestPassword commonconfig.Secret `json:"dest_password" env:"RESTORE_DEST_PASSWORD" desc:"Password or app token of the user."`
	DestBearer   commonconfig.Secret `json:"dest_bearer" env:"RESTORE_DEST_BEARER" desc:"Bearer token, instead of a user."`
	DestInsecure bool                `json:"dest_insecure" env:"RESTORE_DEST_INSECURE" desc:"Do not verify the TLS certificate of the destination."`
}

// runRestore writes the files of the tree to a WebDAV endpoint, reading
// their blobs from the bucket of the platform.
func runRestore(ctx context.Context, version string, args []string) error {
	flags := newFlags("restore")
	to := flags.String("to", "", "URL template of the destination; {space_id} and {space_name} stand for the space. Required.")
	var spaces, maps repeatable
	flags.Var(&spaces, "space", "Limit the run to this space id; may be given more than once.")
	flags.Var(&maps, "space-map", "old=new: write the space known as old into the space new of the destination; may be given more than once.")
	prefix := flags.String("prefix", "/", "Limit the run to the files under this path of each space.")
	workers := flags.Int("workers", defaultWorkers, "How many files travel at once.")
	dryRun := flags.Bool("dry-run", false, "List what would be written and write nothing.")
	skipExisting := flags.Bool("skip-existing", false, "Leave alone what the destination already holds with the same size.")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		return errors.New("restore needs -to, the URL of the destination")
	}

	spaceMap := map[string]string{}
	for _, pair := range maps {
		old, updated, found := strings.Cut(pair, "=")
		if !found || old == "" || updated == "" {
			return fmt.Errorf("-space-map %q: want old=new", pair)
		}
		spaceMap[old] = updated
	}

	t, err := openTools(ctx, version, false)
	if err != nil {
		return err
	}
	defer t.Close()

	var rc restoreConfig
	if err := commonconfig.Decode(&rc); err != nil {
		return err
	}
	if rc.BlobsEndpoint == "" {
		rc.BlobsEndpoint = t.cfg.Tree.Endpoint
	}
	if rc.BlobsRegion == "" {
		rc.BlobsRegion = t.cfg.Tree.Region
	}
	if rc.BlobsAccessKey == "" {
		rc.BlobsAccessKey, rc.BlobsSecretKey = t.cfg.Tree.AccessKey, t.cfg.Tree.SecretKey
	}
	blobs, err := s3store.New(s3store.Config{
		Endpoint:  rc.BlobsEndpoint,
		Region:    rc.BlobsRegion,
		Bucket:    rc.BlobsBucket,
		AccessKey: rc.BlobsAccessKey,
		SecretKey: rc.BlobsSecretKey,
	})
	if err != nil {
		return err
	}
	if err := blobs.Ping(ctx); err != nil {
		return err
	}

	creds := restore.Credentials{User: rc.DestUser, Password: rc.DestPassword.Reveal(), Bearer: rc.DestBearer.Reveal()}
	if creds.User == "" && creds.Bearer == "" {
		return restore.ErrNoCredentials
	}
	sink, err := restore.NewWebDAV(restore.Target{Template: *to, SpaceMap: spaceMap}, creds, rc.DestInsecure)
	if err != nil {
		return err
	}

	report, err := restore.Run(ctx, t.tree, blobs, sink, restore.Options{
		Spaces:       spaces,
		Prefix:       *prefix,
		Workers:      *workers,
		DryRun:       *dryRun,
		SkipExisting: *skipExisting,
	}, t.log)
	if report != nil {
		printRestore(report, *dryRun)
	}
	if err != nil {
		return err
	}
	if !report.OK() {
		return errors.New("restore is incomplete: see the files listed above")
	}
	return nil
}

func printRestore(report *restore.Report, dryRun bool) {
	verb := "restored"
	if dryRun {
		verb = "to restore"
	}
	fmt.Fprintf(os.Stdout, "spaces %d, %s %d, skipped %d, failed %d, without blob %d\n",
		report.Spaces, verb, report.Restored, report.Skipped, report.Failed, len(report.NoBlob))
	for _, file := range report.NoBlob {
		fmt.Fprintf(os.Stdout, "no blob: %s\n", file)
	}
	for _, failure := range report.Errors {
		fmt.Fprintf(os.Stdout, "failed: %s\n", failure)
	}
}
