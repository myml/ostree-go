package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/pkg/gpg"
	"github.com/myml/ostree-go/pkg/repo"
	"github.com/myml/ostree-go/pkg/sign"
)

// remoteGpgImportOptions mirrors ot-remote-builtin-gpg-import.c.
func remoteGpgImportOptions() []cli.Option {
	return []cli.Option{
		{LongName: "keyring", ShortName: 'k', ArgType: cli.ArgFilenameArray, ArgDescription: "FILE", Description: "Import keys from a keyring file (repeatable)"},
		{LongName: "stdin", ArgType: cli.ArgNone, Description: "Import keys from standard input"},
	}
}

// remoteGpgListKeysOptions mirrors ot-remote-builtin-gpg-list-keys.c.
func remoteGpgListKeysOptions() []cli.Option {
	return nil
}

func runRemoteGpgImport(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("NAME must be specified")
	}
	if ctx.Options.Has("stdin") && ctx.Options.Has("keyring") {
		return fmt.Errorf("--keyring and --stdin are mutually exclusive")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	name := ctx.Args[0]
	opts := gpg.ImportOptions{
		RepoPath: r.Path(),
		Remote:   name,
		KeyFiles: ctx.Options.Strings("keyring"),
		KeyIDs:   ctx.Args[1:],
	}
	if ctx.Options.Has("stdin") {
		opts.Stdin = ctx.Stdin
	}
	imported, err := gpg.Import(opts)
	if err != nil {
		return err
	}
	plural := "s"
	if imported == 1 {
		plural = ""
	}
	fmt.Fprintf(ctx.Stdout, "Imported %d GPG key%s to remote \"%s\"\n", imported, plural, name)
	return nil
}

func runRemoteGpgListKeys(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	name := ""
	if len(ctx.Args) > 0 {
		name = ctx.Args[0]
	}
	keys, err := gpg.ListKeys(r.Path(), name)
	if err != nil {
		return err
	}
	for _, key := range keys {
		printGPGKey(ctx, key)
	}
	return nil
}

// printGPGKey mirrors ot_dump_gpg_key().
func printGPGKey(ctx *cli.Context, key *gpg.Key) {
	for i, sub := range key.Subkeys {
		primary := i == 0
		prefix := "    "
		label := "  Subkey"
		if primary {
			prefix = "  "
			label = "Key"
		}
		note := ""
		if sub.Revoked {
			note += " (revoked)"
		}
		if sub.Invalid {
			note += " (invalid)"
		}
		fmt.Fprintf(ctx.Stdout, "%s: %s%s\n", label, sub.Fingerprint, note)
		fmt.Fprintf(ctx.Stdout, "%sCreated: %s\n", prefix, gpg.FormatTime(sub.Created))
		if sub.Expires > 0 {
			kind := "Expires"
			if sub.Expired {
				kind = "Expired"
			}
			fmt.Fprintf(ctx.Stdout, "%s%s: %s\n", prefix, kind, gpg.FormatTime(sub.Expires))
		}
		if !primary {
			continue
		}
		for _, uid := range key.UIDs {
			unote := ""
			if uid.Revoked {
				unote += " (revoked)"
			}
			if uid.Invalid {
				unote += " (invalid)"
			}
			fmt.Fprintf(ctx.Stdout, "  UID: %s%s\n", uid.Value, unote)
			fmt.Fprintf(ctx.Stdout, "  Advanced update URL: %s\n", uid.AdvancedURL)
			fmt.Fprintf(ctx.Stdout, "  Direct update URL: %s\n", uid.DirectURL)
		}
	}
}

// remoteAddExtraOptions are the options owned by the signing subsystem that
// are registered on `ostree remote add` without touching the transport code.
func remoteAddExtraOptions() []cli.Option {
	return []cli.Option{
		{LongName: "gpg-import", ArgType: cli.ArgFilename, ArgDescription: "FILE", Description: "Import GPG key from FILE"},
		{LongName: "sign-verify", ArgType: cli.ArgStringArray, ArgDescription: "KEYTYPE=[inline|file]:PUBKEY", Description: "Verify signatures using KEYTYPE=inline:PUBKEY or KEYTYPE=file:/path/to/key"},
	}
}

// verifyOption is a parsed --sign-verify=KEYTYPE=[inline|file]:DATA option.
type verifyOption struct {
	keyType string
	keyRef  string
	data    string
}

// parseVerifyOption mirrors add_verify_opt() in ot-remote-builtin-add.c.
func parseVerifyOption(spec string) (verifyOption, error) {
	parts := strings.SplitN(spec, "=", 2)
	keyType := parts[0]
	if len(parts) < 2 {
		return verifyOption{}, fmt.Errorf("Failed to parse KEYTYPE=[inline|file]:DATA in %s", spec)
	}
	if _, err := sign.GetByName(keyType); err != nil {
		return verifyOption{}, err
	}
	keyParts := strings.SplitN(parts[1], ":", 2)
	keyRef := keyParts[0]
	if keyRef != "inline" && keyRef != "file" {
		return verifyOption{}, fmt.Errorf("Invalid key reference %s, expected inline|file", keyRef)
	}
	if len(keyParts) < 2 {
		return verifyOption{}, fmt.Errorf("Failed to parse KEYTYPE=[inline|file]:DATA in %s", spec)
	}
	return verifyOption{keyType: keyType, keyRef: keyRef, data: keyParts[1]}, nil
}

// runRemoteAddWithExtras wraps runRemoteAdd so the signing options can be
// handled without modifying the transport's remote-add implementation.
func runRemoteAddWithExtras(ctx *cli.Context) error {
	specs := ctx.Options.Strings("sign-verify")
	parsed := make([]verifyOption, 0, len(specs))
	for _, spec := range specs {
		opt, err := parseVerifyOption(spec)
		if err != nil {
			return err
		}
		parsed = append(parsed, opt)
	}
	if err := runRemoteAdd(ctx); err != nil {
		return err
	}
	if len(parsed) > 0 {
		r, err := repo.Open(remoteRepoPath(ctx))
		if err != nil {
			return fmt.Errorf("opening repo: %w", err)
		}
		cfg := r.Config()
		name := ctx.Args[0]
		var types []string
		for _, opt := range parsed {
			key := "verification-" + opt.keyType + "-key"
			if opt.keyRef == "file" {
				key = "verification-" + opt.keyType + "-file"
			}
			cfg.SetRemote(name, key, opt.data)
			types = append(types, opt.keyType)
		}
		cfg.SetRemote(name, "sign-verify", strings.Join(types, ","))
		if err := saveConfig(r, cfg); err != nil {
			return err
		}
	}
	if file := ctx.Options.String("gpg-import"); file != "" {
		r, err := repo.Open(remoteRepoPath(ctx))
		if err != nil {
			return fmt.Errorf("opening repo: %w", err)
		}
		name := ctx.Args[0]
		imported, err := gpg.Import(gpg.ImportOptions{
			RepoPath: r.Path(),
			Remote:   name,
			KeyFiles: []string{file},
		})
		if err != nil {
			return err
		}
		plural := "s"
		if imported == 1 {
			plural = ""
		}
		fmt.Fprintf(ctx.Stdout, "Imported %d GPG key%s to remote \"%s\"\n", imported, plural, name)
	}
	return nil
}

// runRemoteDeleteWithKeyring wraps runRemoteDelete so the per-remote GPG
// keyring is removed along with the remote configuration.
func runRemoteDeleteWithKeyring(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("NAME must be specified")
	}
	name := ctx.Args[0]
	if err := runRemoteDelete(ctx); err != nil {
		return err
	}
	r, err := repo.Open(remoteRepoPath(ctx))
	if err != nil {
		return fmt.Errorf("opening repo: %w", err)
	}
	keyring := gpg.RemoteKeyringPath(r.Path(), name)
	if err := os.Remove(keyring); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
