package cmd

import (
	"fmt"
	"os"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/internal/repo"
	"github.com/myml/ostree-go/internal/sign"
)

// signOptions mirrors ot-builtin-sign.c.
func signOptions() []cli.Option {
	return []cli.Option{
		{LongName: "delete", ShortName: 'd', ArgType: cli.ArgNone, Description: "Delete signatures having any of the KEY-IDs"},
		{LongName: "verify", ArgType: cli.ArgNone, Description: "Verify signatures"},
		{LongName: "sign-type", ShortName: 's', ArgType: cli.ArgString, ArgDescription: "NAME", Description: "Signature type to use (defaults to 'ed25519')"},
		{LongName: "keys-file", ArgType: cli.ArgFilename, ArgDescription: "NAME", Description: "Read keys from file"},
		{LongName: "keys-dir", ArgType: cli.ArgFilename, ArgDescription: "NAME", Description: "Redefine system-wide directories with public and revoked keys for verification"},
	}
}

func runSign(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("Need a COMMIT to sign or verify")
	}
	verify := ctx.Options.Has("verify")
	keysFile := ctx.Options.String("keys-file")
	keysDir := ctx.Options.String("keys-dir")
	if !verify && keysFile == "" && len(ctx.Args) < 2 {
		return fmt.Errorf("Need at least one KEY-ID to sign with")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	commit, err := resolveRevision(r, ctx.Args[0])
	if err != nil {
		return err
	}
	keyIDs := ctx.Args[1:]
	name := ctx.Options.String("sign-type")
	if name == "" {
		name = sign.NameEd25519
	}
	engine, err := sign.GetByName(name)
	if err != nil {
		return err
	}

	if verify {
		return verifyCommitWithSign(ctx, r, engine, commit, keyIDs, keysFile, keysDir)
	}

	for _, keyID := range keyIDs {
		if err := engine.SetSK(keyID); err != nil {
			return err
		}
		if err := sign.SignCommit(r, engine, commit); err != nil {
			return err
		}
	}
	if keysFile != "" {
		blobs, err := readSecretKeyFile(engine.Name(), keysFile)
		if err != nil {
			return err
		}
		for _, blob := range blobs {
			if err := engine.SetSKBytes(blob); err != nil {
				return err
			}
			if err := sign.SignCommit(r, engine, commit); err != nil {
				return err
			}
		}
	}
	return nil
}

// verifyCommitWithSign implements `ostree sign --verify`.
func verifyCommitWithSign(ctx *cli.Context, r *repo.Repo, engine *sign.Engine, commit string, keyIDs []string, keysFile, keysDir string) error {
	for _, keyID := range keyIDs {
		if err := engine.SetPK(keyID); err != nil {
			continue
		}
		msg, err := sign.VerifyCommit(r, engine, commit)
		if err == nil {
			fmt.Fprintln(ctx.Stdout, msg)
			return nil
		}
	}
	if len(keyIDs) == 0 || keysFile != "" {
		fresh, err := sign.GetByName(engine.Name())
		if err != nil {
			return err
		}
		if err := fresh.LoadPK(sign.LoadPKOptions{Filename: keysFile, BaseDir: keysDir}); err != nil {
			return err
		}
		msg, err := sign.VerifyCommit(r, fresh, commit)
		if err != nil {
			return err
		}
		fmt.Fprintln(ctx.Stdout, msg)
		return nil
	}
	return fmt.Errorf("No valid signatures found")
}

// readSecretKeyFile decodes a private key file into raw keys.  ed25519 keys
// are base64 encoded one per line; spki keys are PEM "PRIVATE KEY" blocks.
func readSecretKeyFile(name, filename string) ([][]byte, error) {
	st, err := os.Stat(filename)
	if err != nil || st.IsDir() {
		return nil, fmt.Errorf("File object '%s' is not a regular file", filename)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	return sign.DecodeKeyBlobs(name, data, false)
}
