package cmd

import (
	"fmt"
	"sort"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/pkg/object"
	"github.com/myml/ostree-go/pkg/pull"
)

// remoteRefsOptions mirrors ot-remote-builtin-refs.c.
func remoteRefsOptions() []cli.Option {
	return []cli.Option{
		{LongName: "revision", ShortName: 'r', ArgType: cli.ArgNone, Description: "Show revisions in listing"},
		{LongName: "cache-dir", ArgType: cli.ArgFilename, ArgDescription: "DIR", Description: "Use custom cache dir"},
	}
}

// runRemoteRefs implements `ostree remote refs NAME`: list the refs of the
// remote's summary file as NAME:REF (optionally NAME:REF<TAB>CHECKSUM).
func runRemoteRefs(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("NAME must be specified")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	name := ctx.Args[0]
	summaryData, err := pull.FetchRemoteSummary(r, name, ctx.Options.String("cache-dir"))
	if err != nil {
		return err
	}
	if len(summaryData) == 0 {
		return fmt.Errorf("Remote refs not available; server has no summary file")
	}
	summary, err := object.ParseSummary(summaryData)
	if err != nil {
		return err
	}
	refs := append([]object.SummaryRef(nil), summary.Refs...)
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	for _, ref := range refs {
		if ctx.Options.Has("revision") {
			fmt.Fprintf(ctx.Stdout, "%s:%s\t%s\n", name, ref.Name, ref.Commit)
		} else {
			fmt.Fprintf(ctx.Stdout, "%s:%s\n", name, ref.Name)
		}
	}
	return nil
}
