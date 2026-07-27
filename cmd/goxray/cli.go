package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"go.inndy.tw/goxray/internal/config"
	"go.inndy.tw/goxray/internal/store"
)

// runCLI dispatches the read-only query subcommands.
func runCLI(cmd string, args []string) error {
	cfg, err := config.Load(nil) // resolve home/paths from env; ignore serve-only flags
	if err != nil {
		return err
	}
	rd, err := store.OpenReader(cfg.BlobsDir(), cfg.ManifestsDir(), cfg.IndexPath())
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
	}
	return fmt.Errorf("unknown command %q", cmd)
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
	data, err := rd.Blob(ref)
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
