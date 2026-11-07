package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/pkg/sign"
	"github.com/myml/ostree-go/pkg/staticdelta"
)

// registerStaticDelta wires the implemented static-delta subcommands.
// The command table (names, descriptions and usage strings) lives in
// internal/cli/command.go; only options and Run functions are attached
// here.
func registerStaticDelta(root *cli.Command) {
	for _, sub := range root.Subcommands {
		switch sub.Name {
		case "list":
			sub.Run = runStaticDeltaList
		case "show":
			sub.Run = runStaticDeltaShow
		case "delete":
			sub.Run = runStaticDeltaDelete
		case "generate":
			sub.Options = staticDeltaGenerateOptions()
			sub.Run = runStaticDeltaGenerate
		case "apply-offline":
			sub.Options = staticDeltaSignatureOptions("apply-offline")
			sub.Run = runStaticDeltaApplyOffline
		case "verify":
			sub.Options = staticDeltaSignatureOptions("verify")
			sub.Run = runStaticDeltaVerify
		case "indexes":
			sub.Run = runStaticDeltaIndexes
		case "reindex":
			sub.Options = []cli.Option{
				{LongName: "to", ArgType: cli.ArgString, ArgDescription: "REV",
					Description: "Only update delta index to revision REV"},
			}
			sub.Run = runStaticDeltaReindex
		}
	}
}

// staticDeltaSignatureOptions mirrors apply_offline_options[] and
// verify_options[] in ot-builtin-static-delta.c.
func staticDeltaSignatureOptions(command string) []cli.Option {
	opts := []cli.Option{
		{LongName: "sign-type", ArgType: cli.ArgString, ArgDescription: "NAME",
			Description: "Signature type to use (defaults to 'ed25519')"},
		{LongName: "keys-file", ArgType: cli.ArgString, ArgDescription: "NAME",
			Description: "Read key(s) from file"},
	}
	if command == "apply-offline" || command == "verify" {
		opts = append(opts, cli.Option{
			LongName: "keys-dir", ArgType: cli.ArgString, ArgDescription: "NAME",
			Description: "Redefine system-wide directories with public and revoked keys for verification",
		})
	}
	return opts
}

// staticDeltaGenerateOptions mirrors generate_options[] in
// src/ostree/ot-builtin-static-delta.c.
func staticDeltaGenerateOptions() []cli.Option {
	return []cli.Option{
		{LongName: "from", ArgType: cli.ArgString, ArgDescription: "REV",
			Description: "Create delta from revision REV"},
		{LongName: "empty", ArgType: cli.ArgNone,
			Description: "Create delta from scratch"},
		{LongName: "inline", ArgType: cli.ArgNone,
			Description: "Inline delta parts into main delta"},
		{LongName: "to", ArgType: cli.ArgString, ArgDescription: "REV",
			Description: "Create delta to revision REV"},
		{LongName: "disable-bsdiff", ArgType: cli.ArgNone,
			Description: "Disable use of bsdiff"},
		{LongName: "if-not-exists", ShortName: 'n', ArgType: cli.ArgNone,
			Description: "Only generate if a delta does not already exist"},
		{LongName: "set-endianness", ArgType: cli.ArgString, ArgDescription: "ENDIAN",
			Description: "Choose metadata endianness ('l' or 'B')"},
		{LongName: "swap-endianness", ArgType: cli.ArgNone,
			Description: "Swap metadata endianness from host order"},
		{LongName: "min-fallback-size", ArgType: cli.ArgString,
			ArgDescription: "MB",
			Description:    "Minimum uncompressed size in megabytes for individual HTTP request"},
		{LongName: "max-bsdiff-size", ArgType: cli.ArgString, ArgDescription: "MB",
			Description: "Maximum size in megabytes to consider bsdiff compression for input files"},
		{LongName: "max-chunk-size", ArgType: cli.ArgString, ArgDescription: "MB",
			Description: "Maximum size of delta chunks in megabytes"},
		{LongName: "filename", ArgType: cli.ArgFilename, ArgDescription: "PATH",
			Description: "Write the delta content to PATH (a directory).  If not specified, the OSTree repository is used"},
		{LongName: "sign", ArgType: cli.ArgStringArray, ArgDescription: "KEY_ID",
			Description: "Sign the delta with"},
		{LongName: "sign-type", ArgType: cli.ArgString, ArgDescription: "NAME",
			Description: "Signature type to use (defaults to 'ed25519')"},
		{LongName: "keys-file", ArgType: cli.ArgString, ArgDescription: "NAME",
			Description: "Read key(s) from file"},
	}
}

func runStaticDeltaList(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	names, err := staticdelta.List(r)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintln(ctx.Stdout, "(No static deltas)")
		return nil
	}
	for _, name := range names {
		fmt.Fprintln(ctx.Stdout, name)
	}
	return nil
}

// staticDeltaArgs drops the subcommand name, which libostree's legacy
// static-delta dispatch leaves in argv.
func staticDeltaArgs(ctx *cli.Context) []string {
	if len(ctx.Args) > 0 && ctx.Command != nil && ctx.Args[0] == ctx.Command.Name {
		return ctx.Args[1:]
	}
	return ctx.Args
}

func runStaticDeltaShow(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	args := staticDeltaArgs(ctx)
	if len(args) < 1 {
		return fmt.Errorf("DELTA must be specified")
	}
	return staticdelta.Show(r, ctx.Stdout, args[0])
}

func runStaticDeltaDelete(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	args := staticDeltaArgs(ctx)
	if len(args) < 1 {
		return fmt.Errorf("DELTA must be specified")
	}
	return staticdelta.Delete(r, args[0])
}

func runStaticDeltaGenerate(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	if !r.Writable() {
		if err := r.WritableError(); err != nil {
			return err
		}
		return fmt.Errorf("Repository is not writable")
	}

	args := staticDeltaArgs(ctx)
	toRev := ctx.Options.String("to")
	if len(args) > 1 {
		return fmt.Errorf("Too many arguments")
	}
	if len(args) == 1 && toRev == "" {
		toRev = args[0]
	}
	if toRev == "" {
		return fmt.Errorf("TO revision must be specified")
	}

	fromRev := ctx.Options.String("from")
	empty := ctx.Options.Has("empty")
	if empty && fromRev != "" {
		return fmt.Errorf("Cannot specify both --empty and --from=REV")
	}
	if empty {
		fromRev = ""
	} else if fromRev == "" {
		// Like the C CLI, default to the parent of the target.
		fromRev = toRev + "^"
	}

	signKeys, err := staticDeltaSignKeys(ctx)
	if err != nil {
		return err
	}

	maxChunk := staticdelta.DefaultMaxChunkSize
	if raw := ctx.Options.String("max-chunk-size"); raw != "" {
		maxChunk = parseMegabytes(raw)
	}
	// libostree's default --max-bsdiff-size is 128 MB; --disable-bsdiff
	// forces it off and an explicit value overrides the default.
	maxBsdiff := uint64(128 * 1000 * 1000)
	if ctx.Options.Has("disable-bsdiff") {
		maxBsdiff = 0
	} else if raw := ctx.Options.String("max-bsdiff-size"); raw != "" {
		maxBsdiff = parseMegabytes(raw)
	}

	var target staticdelta.Endianness
	targetSet := false
	if e := ctx.Options.String("set-endianness"); e != "" {
		switch e {
		case "l":
			target, targetSet = staticdelta.EndianLittle, true
		case "B":
			target, targetSet = staticdelta.EndianBig, true
		default:
			return fmt.Errorf("Invalid endianness '%s'", e)
		}
	}
	if ctx.Options.Has("swap-endianness") {
		if targetSet {
			if target == staticdelta.EndianLittle {
				target = staticdelta.EndianBig
			} else {
				target = staticdelta.EndianLittle
			}
		} else {
			if staticdelta.HostEndianness() == staticdelta.EndianLittle {
				target = staticdelta.EndianBig
			} else {
				target = staticdelta.EndianLittle
			}
			targetSet = true
		}
	}

	fromDisplay := "empty"
	if fromRev != "" {
		resolved, err := staticdelta.ResolveRev(r, fromRev)
		if err != nil {
			return err
		}
		fromDisplay = resolved
	}
	toResolved, err := staticdelta.ResolveRev(r, toRev)
	if err != nil {
		return err
	}

	fmt.Fprintln(ctx.Stdout, "Generating static delta:")
	fmt.Fprintf(ctx.Stdout, "  From: %s\n", fromDisplay)
	fmt.Fprintf(ctx.Stdout, "  To:   %s\n", toResolved)

	res, err := staticdelta.Generate(staticdelta.GenerateOptions{
		Repo:          r,
		From:          fromRev,
		To:            toResolved,
		Inline:        ctx.Options.Has("inline"),
		Filename:      ctx.Options.String("filename"),
		MaxChunkSize:  maxChunk,
		Verbose:       true,
		Log:           ctx.Stderr,
		IfNotExists:   ctx.Options.Has("if-not-exists"),
		Endianness:    target,
		EndiannessSet: targetSet,
		SignKeys:      signKeys,
		SignType:      ctx.Options.String("sign-type"),
		MaxBsdiffSize: maxBsdiff,
	})
	if err != nil {
		return err
	}
	if res.Existed {
		fmt.Fprintf(ctx.Stdout, "Delta %s already exists.\n", res.Name)
	}
	return nil
}

// staticDeltaSignKeys collects --sign keys and any secrets listed in
// --keys-file.  Empty key files do not enable signing.
func staticDeltaSignKeys(ctx *cli.Context) ([]string, error) {
	keys := append([]string(nil), ctx.Options.Strings("sign")...)
	name := ctx.Options.String("keys-file")
	if name == "" {
		return keys, nil
	}
	fi, err := os.Stat(name)
	if err != nil || !fi.Mode().IsRegular() {
		fmt.Fprintf(ctx.Stderr, "Can't open file '%s' with keys\n", name)
		return nil, fmt.Errorf("File object '%s' is not a regular file", name)
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line != "" {
			keys = append(keys, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

// staticDeltaSignatureEngine builds the engine used by apply-offline and
// verify.  It mirrors the C behavior of defaulting to ed25519.
func staticDeltaSignatureEngine(ctx *cli.Context, keyIDs []string) (*sign.Engine, error) {
	name := ctx.Options.String("sign-type")
	if name == "" {
		name = sign.NameEd25519
	}
	engine, err := sign.GetByName(name)
	if err != nil {
		return nil, err
	}
	hasKey := false
	for _, id := range keyIDs {
		if err := engine.AddPK(id); err != nil {
			return nil, err
		}
		hasKey = true
	}
	if keysFile := ctx.Options.String("keys-file"); keysFile != "" {
		if err := engine.LoadPK(sign.LoadPKOptions{Filename: keysFile}); err != nil {
			return nil, err
		}
		hasKey = true
	}
	if keysDir := ctx.Options.String("keys-dir"); keysDir != "" {
		if err := engine.LoadPK(sign.LoadPKOptions{BaseDir: keysDir}); err != nil {
			return nil, err
		}
		hasKey = true
	}
	if !hasKey {
		// libostree falls back to the system key directories; when that
		// fails it considers the signing engine unavailable.
		if err := engine.LoadPK(sign.LoadPKOptions{}); err != nil {
			engine.ClearKeys()
			return nil, nil
		}
	}
	return engine, nil
}

func runStaticDeltaVerify(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	args := staticDeltaArgs(ctx)
	if len(args) < 1 {
		return fmt.Errorf("DELTA must be specified")
	}
	engine, err := staticDeltaSignatureEngine(ctx, args[1:])
	if err != nil {
		fmt.Fprintln(ctx.Stdout, "Sign-type not supported")
		return err
	}
	if engine == nil {
		fmt.Fprintln(ctx.Stdout, "Sign-type not supported")
		return fmt.Errorf("Requested signature type is not implemented")
	}
	msg, err := staticdelta.VerifySignature(r, args[0], engine)
	if err != nil {
		fmt.Fprintln(ctx.Stdout, "Verification fails")
		return err
	}
	_ = msg
	fmt.Fprintln(ctx.Stdout, "Verification OK")
	return nil
}

func runStaticDeltaApplyOffline(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	args := staticDeltaArgs(ctx)
	if len(args) < 1 {
		return fmt.Errorf("PATH must be specified")
	}
	engine, err := staticDeltaSignatureEngine(ctx, args[1:])
	if err != nil {
		return err
	}
	return staticdelta.ApplyOffline(staticdelta.ApplyOfflineOptions{
		Repo: r,
		Path: args[0],
		Sign: engine,
	})
}

func runStaticDeltaIndexes(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	indexes, err := staticdelta.ListIndexes(r)
	if err != nil {
		return err
	}
	if len(indexes) == 0 {
		fmt.Fprintln(ctx.Stdout, "(No static deltas indexes)")
		return nil
	}
	for _, index := range indexes {
		fmt.Fprintln(ctx.Stdout, index)
	}
	return nil
}

func runStaticDeltaReindex(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	if !r.Writable() {
		if err := r.WritableError(); err != nil {
			return err
		}
		return fmt.Errorf("Repository is not writable")
	}
	return staticdelta.Reindex(r, staticdelta.ReindexOptions{
		ToCommit: ctx.Options.String("to"),
	})
}

// parseMegabytes mirrors the C CLI's g_ascii_strtoull(value) * 1000 * 1000.
func parseMegabytes(raw string) uint64 {
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return n * 1000 * 1000
}

var _ = os.Stderr
