package cmd

import (
	"fmt"
	"io"
	"sort"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/internal/gvariant"
	"github.com/myml/ostree-go/internal/object"
	"github.com/myml/ostree-go/internal/pull"
)

// remoteSummaryOptions mirrors ot-remote-builtin-summary.c.
func remoteSummaryOptions() []cli.Option {
	return []cli.Option{
		{LongName: "list-metadata-keys", ArgType: cli.ArgNone, Description: "List the available metadata keys"},
		{LongName: "print-metadata-key", ArgType: cli.ArgString, ArgDescription: "KEY", Description: "Print string value of metadata key"},
		{LongName: "cache-dir", ArgType: cli.ArgFilename, ArgDescription: "DIR", Description: "Use custom cache dir"},
		{LongName: "raw", ArgType: cli.ArgNone, Description: "Show raw variant data"},
	}
}

// runRemoteSummaryDump implements `ostree remote summary`, printing the
// remote summary in the same format as ot_dump_summary_bytes().
func runRemoteSummaryDump(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("NAME must be specified")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	summaryData, err := pull.FetchRemoteSummary(r, ctx.Args[0], ctx.Options.String("cache-dir"))
	if err != nil {
		return err
	}
	if len(summaryData) == 0 {
		return fmt.Errorf("Remote server has no summary file")
	}
	if ctx.Options.Has("raw") {
		_, err := ctx.Stdout.Write(summaryData)
		return err
	}
	s, err := object.ParseSummary(summaryData)
	if err != nil {
		return err
	}
	switch {
	case ctx.Options.Has("list-metadata-keys"):
		keys := dictKeys(s.Metadata)
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintln(ctx.Stdout, key)
		}
		return nil
	case ctx.Options.String("print-metadata-key") != "":
		key := ctx.Options.String("print-metadata-key")
		v, ok := s.Metadata.Get(key)
		if !ok {
			return fmt.Errorf("No such metadata key '%s'", key)
		}
		variant, ok := v.(gvariant.Variant)
		if !ok {
			return fmt.Errorf("No such metadata key '%s'", key)
		}
		formatted, err := gvFormat(variant.Type, variant.Value, true, hostIsBigEndian())
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(ctx.Stdout, formatted)
		return err
	}
	dumpSummaryObject(ctx.Stdout, s)
	return nil
}

// dumpSummaryObject renders a parsed summary like ot_dump_summary_bytes().
func dumpSummaryObject(w io.Writer, s *object.Summary) {
	for _, ref := range s.Refs {
		fmt.Fprintf(w, "* %s\n", ref.Name)
		fmt.Fprintf(w, "    Latest Commit (%d bytes):\n", ref.Size)
		fmt.Fprintf(w, "      %s\n", ref.Commit)
		if v, ok := ref.Metadata.Get(object.MetaSummaryCommitTimestamp); ok {
			fmt.Fprintf(w, "    Timestamp (%s): %s\n", object.MetaSummaryCommitTimestamp, formatSummaryTime(metadataUint64(v)))
		}
		if v, ok := ref.Metadata.Get(object.MetaSummaryCommitVersion); ok {
			version, _ := variantString(v)
			fmt.Fprintf(w, "    Version (%s): %s\n", object.MetaSummaryCommitVersion, version)
		}
		fmt.Fprintln(w)
	}
	for _, key := range dictKeys(s.Metadata) {
		v, _ := s.Metadata.Get(key)
		val := formatSummaryValue(v)
		switch key {
		case object.MetaSummaryMode:
			fmt.Fprintf(w, "Repository Mode (%s): %s\n", key, val)
		case object.MetaSummaryLastModified:
			fmt.Fprintf(w, "Last-Modified (%s): %s\n", key, formatSummaryTime(metadataUint64(v)))
		case object.MetaSummaryExpires:
			fmt.Fprintf(w, "Expires (%s): %s\n", key, formatSummaryTime(metadataUint64(v)))
		case object.MetaSummaryTombstoneCommits:
			yes := "No"
			if b, ok := variantBool(v); ok && b {
				yes = "Yes"
			}
			fmt.Fprintf(w, "Has Tombstone Commits (%s): %s\n", key, yes)
		case object.MetaSummaryStaticDeltas:
			fmt.Fprintf(w, "Static Deltas (%s): %s\n", key, val)
		default:
			fmt.Fprintf(w, "%s: %s\n", key, val)
		}
	}
}
