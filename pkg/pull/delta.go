package pull

import (
	"fmt"
	"strings"

	"github.com/myml/ostree-go/pkg/checksum"
	"github.com/myml/ostree-go/pkg/gvariant"
	"github.com/myml/ostree-go/pkg/object"
	"github.com/myml/ostree-go/pkg/repo"
	"github.com/myml/ostree-go/pkg/staticdelta"
)

type deltaCandidate struct {
	name string
	sb   *staticdelta.Superblock
}

// findDelta picks a static delta from the remote summary that ends at target.
// A delta whose source commit is not present locally is skipped, matching the
// "best delta" selection in libostree.
func findDelta(f *fetcher, opts RemoteOptions, summary []byte, target string) (*deltaCandidate, error) {
	parsed, err := object.ParseSummary(summary)
	if err != nil {
		return nil, err
	}
	v, ok := parsed.Metadata.Get(object.MetaSummaryStaticDeltas)
	if !ok {
		return nil, nil
	}
	variant, ok := v.(gvariant.Variant)
	if !ok {
		return nil, nil
	}
	dict, ok := variant.Value.(gvariant.Dict)
	if !ok {
		return nil, nil
	}
	for _, e := range dict {
		name, _ := e.Key.(string)
		if name != target && !strings.HasSuffix(name, "-"+target) {
			continue
		}
		from, to, perr := staticdelta.ParseDeltaName(name)
		if perr != nil {
			continue
		}
		sbPath, perr := staticdelta.DeltaSuperblockPath(from, to)
		if perr != nil {
			continue
		}
		data, err := f.get(sbPath)
		if err != nil {
			continue
		}
		sb, err := staticdelta.ParseSuperblock(data)
		if err != nil {
			continue
		}
		if checksum.Hex(sb.To) != target {
			continue
		}
		if sb.HasFrom {
			from := checksum.Hex(sb.From)
			have, err := opts.Repo.HasObject(from, repo.ObjectCommit)
			if err != nil || !have {
				continue
			}
		}
		return &deltaCandidate{name: name, sb: sb}, nil
	}
	return nil, nil
}

func deltaSizes(sb *staticdelta.Superblock) (compressed, uncompressed uint64) {
	swap := sb.NeedsSwap()
	for _, p := range sb.Parts {
		compressed += staticdelta.Swap64(swap, p.Size)
		uncompressed += staticdelta.Swap64(swap, p.USize)
	}
	return compressed, uncompressed
}

// formatDeltaSize mirrors g_format_size() for the progress line asserted by
// the upstream tests.
func formatDeltaSize(n uint64) string { return staticdelta.FormatSize(n) }

// applyDelta fetches and executes every part of a static delta.
func applyDelta(f *fetcher, opts RemoteOptions, cand *deltaCandidate) error {
	for i, part := range cand.sb.Parts {
		from, to, perr := staticdelta.ParseDeltaName(cand.name)
		if perr != nil {
			return perr
		}
		partPath, perr := staticdelta.DeltaPartPath(from, to, i)
		if perr != nil {
			return perr
		}
		data, err := f.get(partPath)
		if err != nil {
			return err
		}
		if err := staticdelta.VerifyPartChecksum(data, part.Checksum); err != nil {
			return err
		}
		if err := staticdelta.ApplyPartFile(opts.Repo, part.Objects, data); err != nil {
			return err
		}
	}
	commitBytes, err := cand.sb.CommitBytes()
	if err != nil {
		return err
	}
	if err := opts.Repo.WriteRawObject(checksum.Hex(cand.sb.To), repo.ObjectCommit, commitBytes); err != nil {
		return err
	}
	return opts.Repo.ClearPartial(checksum.Hex(cand.sb.To))
}

// pullStaticDelta attempts a static-delta pull for target. It returns false
// when no suitable delta was found.
func pullStaticDelta(f *fetcher, opts RemoteOptions, summary []byte, target string) (bool, error) {
	cand, err := findDelta(f, opts, summary, target)
	if err != nil || cand == nil {
		return false, err
	}
	compressed, uncompressed := deltaSizes(cand.sb)
	if opts.DryRun {
		fmt.Fprintf(opts.Out, "Delta update: 0/%d parts, 0 bytes/%s, %s total uncompressed\n",
			len(cand.sb.Parts), formatDeltaSize(compressed), formatDeltaSize(uncompressed))
		return true, nil
	}
	if err := applyDelta(f, opts, cand); err != nil {
		return false, err
	}
	// The delta carries the commit object but not its detached metadata;
	// verify the commit signature before the caller writes the ref.
	if err := opts.verify.VerifyDeltaCommit(f, opts.Repo, checksum.Hex(cand.sb.To)); err != nil {
		return false, err
	}
	fmt.Fprintf(opts.Out, "Delta update: %d/%d parts, %s/%s, %s total uncompressed\n",
		len(cand.sb.Parts), len(cand.sb.Parts), formatDeltaSize(compressed),
		formatDeltaSize(compressed), formatDeltaSize(uncompressed))
	return true, nil
}
