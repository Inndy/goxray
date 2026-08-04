package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"go.inndy.tw/goxray/internal/config"
	"go.inndy.tw/goxray/internal/store"
	"go.inndy.tw/goxray/internal/wire"
)

// runCLI dispatches the read-only query subcommands.
func runCLI(cmd string, args []string) error {
	cfg, err := config.Load(nil) // resolve home/paths from env; ignore serve-only flags
	if err != nil {
		return err
	}
	// migrate-usage is the one subcommand that rewrites manifests, so it does
	// not open the read-only Reader.
	if cmd == "migrate-usage" {
		return cmdMigrateUsage(cfg, args)
	}

	rd, err := store.OpenReader(cfg.BlobsDir(), cfg.ManifestsDir(), cfg.ArchivesDir(), cfg.IndexPath())
	if err != nil {
		return err
	}
	defer rd.Close()

	switch cmd {
	case "ls":
		return cmdLs(rd, args)
	case "show":
		return cmdShow(rd, args)
	case "raw":
		return cmdRaw(rd, args)
	case "archive":
		return cmdArchive(cfg, rd, args)
	case "fsck":
		return cmdFsck(cfg, rd, args)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// cmdArchive folds quiet sessions into per-session archives, or restores them.
// Dry run unless --apply, in the same shape as migrate-usage.
func cmdArchive(cfg *config.Config, rd *store.Reader, args []string) error {
	fs := flag.NewFlagSet("archive", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "write the changes (default is a dry run)")
	undo := fs.Bool("unarchive", false, "restore archived bodies as individual blobs")
	session := fs.String("session", "", "only this session id")
	quiet := fs.Duration("quiet", config.DefaultArchiveAfter,
		"how long a session must have been idle to be eligible")
	if err := fs.Parse(args); err != nil {
		return err
	}

	o := store.ArchiveOptions{
		BlobsDir:     cfg.BlobsDir(),
		ManifestsDir: cfg.ManifestsDir(),
		ArchivesDir:  cfg.ArchivesDir(),
		Index:        rd,
		Quiet:        *quiet,
		Apply:        *apply,
		Unarchive:    *undo,
	}
	if *session != "" {
		o.Only = []string{*session}
	}

	st, err := store.ArchiveSessions(o)
	if err != nil {
		return err
	}

	verb := "archive"
	if *undo {
		verb = "restore"
	}
	mode := "dry run"
	if *apply {
		mode = "applied"
	}
	fmt.Printf("%s: %d sessions considered, %d to %s (%d bodies, %d manifests), %d skipped, %d failed\n",
		mode, st.Sessions, st.Archived, verb, st.Refs, st.Manifests, st.Skipped, st.Failed)
	switch {
	case st.BytesFrom == 0:
	case *undo && *apply:
		fmt.Printf("restored %s of bodies from %s of archives\n",
			humanBytes(st.BytesTo), humanBytes(st.BytesFrom))
	case *undo:
		fmt.Printf("%s of archives would be expanded back to individual blobs\n", humanBytes(st.BytesFrom))
	case *apply:
		fmt.Printf("%s -> %s (%.1fx smaller)\n", humanBytes(st.BytesFrom), humanBytes(st.BytesTo),
			float64(st.BytesFrom)/float64(max64(st.BytesTo, 1)))
	default:
		fmt.Printf("%s of blob files would be folded in; run with --apply to see the result\n",
			humanBytes(st.BytesFrom))
	}
	if st.Failed > 0 {
		return fmt.Errorf("%d sessions failed; nothing was deleted for those", st.Failed)
	}
	return nil
}

// cmdFsck verifies archive integrity and reports which exchanges any damage
// would affect.
func cmdFsck(cfg *config.Config, rd *store.Reader, args []string) error {
	fs := flag.NewFlagSet("fsck", flag.ContinueOnError)
	session := fs.String("session", "", "only this session id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Fsck(store.FsckOptions{
		BlobsDir:     cfg.BlobsDir(),
		ManifestsDir: cfg.ManifestsDir(),
		ArchivesDir:  cfg.ArchivesDir(),
		Session:      *session,
	})
	if err != nil {
		return err
	}

	fmt.Printf("%d archives checked, %d chunks verified, %d bodies resolved\n",
		st.Archives, st.Chunks, st.Bodies)
	fmt.Printf("%d manifest packs checked, %d manifests resolved\n", st.Packs, st.Packed)
	fmt.Printf("%d manifests cross-checked, %d bodies unreadable\n", st.Manifests, len(st.Missing))
	for _, p := range st.Problems {
		fmt.Println("  !", p)
	}
	for i, m := range st.Missing {
		if i >= 20 {
			fmt.Printf("  … and %d more\n", len(st.Missing)-i)
			break
		}
		fmt.Printf("  ! %s: %s body unreadable (%s)\n", m.Exchange, m.Side, m.Err)
	}
	if len(st.Problems) > 0 || len(st.Missing) > 0 {
		return errors.New("store has problems")
	}
	fmt.Println("ok")
	return nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// cmdMigrateUsage backfills the metadata older captures did not record — the
// cache-creation TTL split and the context window — by re-parsing the stored
// blobs. Dry-run unless --apply is passed.
func cmdMigrateUsage(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("migrate-usage", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "write the changes (default is a dry run)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.MigrateUsage(cfg.BlobsDir(), cfg.ManifestsDir(), cfg.ArchivesDir(), wire.Anthropic{}, *apply)
	if err != nil {
		return err
	}

	mode := "dry run"
	if *apply {
		mode = "applied"
	}
	fmt.Printf("%s: %d scanned, %d to update, %d skipped, %d failed\n",
		mode, st.Scanned, st.Updated, st.Skipped, st.Failed)
	fmt.Printf("cost with current rates: $%.4f -> $%.4f (%+.4f)\n",
		st.CostFrom, st.CostTo, st.CostTo-st.CostFrom)
	if st.Windows > 0 {
		fmt.Printf("context window recovered for %d turn(s)\n", st.Windows)
	}
	if *apply && st.Updated > 0 {
		fmt.Println("delete index.db and restart to reproject the corrected costs")
	}
	return nil
}

func cmdLs(rd *store.Reader, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	session := fs.String("session", "", "filter by session id")
	source := fs.String("source", "", "filter by source slug")
	limit := fs.Int("limit", 50, "max rows")
	noise := fs.Bool("noise", false, "include noise (count_tokens/quota/hello)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rows, err := rd.List(store.Filter{Session: *session, Source: *source, Limit: *limit, IncludeNoise: *noise})
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tSOURCE\tSTATE\tMODEL\tIN\tOUT\tCOST\tMSGS\tSESSION\tNOISE")
	for _, e := range rows {
		noiseMark := ""
		if e.IsNoise {
			noiseMark = "noise"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t$%.4f\t%d\t%s\t%s\n",
			e.ID, e.StartedAt.Format("01-02 15:04:05"), dash(e.Source), e.State, e.Req.Model,
			e.Res.Usage.InputTokens, e.Res.Usage.OutputTokens, e.Res.Cost,
			e.Req.MsgCount, short(e.Req.SessionID), noiseMark)
	}
	return tw.Flush()
}

func cmdShow(rd *store.Reader, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: goxray show <id>")
	}
	ex, err := rd.Manifest(args[0])
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", args[0], err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(ex)
}

func cmdRaw(rd *store.Reader, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: goxray raw <id> [req|res]")
	}
	which := "req"
	if len(args) >= 2 {
		which = args[1]
	}
	ex, err := rd.Manifest(args[0])
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", args[0], err)
	}
	ref := ex.ReqRef
	if which == "res" {
		ref = ex.ResRef
	}
	if ref == "" {
		return fmt.Errorf("no %s blob for %s", which, args[0])
	}
	data, err := rd.Blob(ex.Req.SessionID, ref)
	if err != nil {
		return fmt.Errorf("read blob %s: %w", ref, err)
	}
	_, err = os.Stdout.Write(data)
	return err
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
