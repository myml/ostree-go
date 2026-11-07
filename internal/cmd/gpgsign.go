package cmd

import (
	"fmt"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/pkg/gpg"
	"github.com/myml/ostree-go/pkg/repo"
	"github.com/myml/ostree-go/pkg/sign"
)

// gpgSignOptions mirrors ot-builtin-gpg-sign.c.
func gpgSignOptions() []cli.Option {
	return []cli.Option{
		{LongName: "delete", ShortName: 'd', ArgType: cli.ArgNone, Description: "Delete signatures having any of the GPG KEY-IDs"},
		{LongName: "gpg-homedir", ArgType: cli.ArgFilename, ArgDescription: "HOMEDIR", Description: "GPG Homedir to use when looking for keyrings"},
	}
}

func runGpgSign(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("Need a COMMIT to sign")
	}
	if len(ctx.Args) < 2 {
		return fmt.Errorf("Need at least one GPG KEY-ID to sign with")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	keyIDs := ctx.Args[1:]
	for _, keyID := range keyIDs {
		if err := gpg.ValidateKeyID(keyID); err != nil {
			return fmt.Errorf("Key IDs must be valid")
		}
	}
	commit, err := resolveRevision(r, ctx.Args[0])
	if err != nil {
		return err
	}
	homedir := ctx.Options.String("gpg-homedir")
	if ctx.Options.Has("delete") {
		deleted, err := deleteGpgSignatures(r, commit, keyIDs)
		if err != nil {
			return err
		}
		fmt.Fprintf(ctx.Stdout, "Signatures deleted: %d\n", deleted)
		return nil
	}
	for _, keyID := range keyIDs {
		if err := gpgSignCommit(r, commit, keyID, homedir); err != nil {
			return err
		}
	}
	return nil
}

// gpgSignCommit appends a GPG signature to a commit's detached metadata.
func gpgSignCommit(r *repo.Repo, commit, keyID, homedir string) error {
	commitData, err := r.ReadObject(commit, repo.ObjectCommit)
	if err != nil {
		return fmt.Errorf("Failed to read commit: %w", err)
	}
	dict, err := sign.ReadDetachedMetadata(r, commit)
	if err != nil {
		return err
	}
	if dict == nil {
		dict = nil
	}
	raw, err := sign.DetachedMetadataSignatures(dict, "ostree.gpgsigs")
	if err != nil {
		return err
	}
	if gpg.SignaturesMatchKeyID(raw, keyID) {
		return fmt.Errorf("Commit is already signed with GPG key %s", keyID)
	}
	sig, err := gpg.SignData(commitData, keyID, homedir)
	if err != nil {
		return err
	}
	return sign.AppendDetachedSignature(r, commit, dict, "ostree.gpgsigs", sig)
}

// deleteGpgSignatures removes signatures issued by keyIDs.
func deleteGpgSignatures(r *repo.Repo, commit string, keyIDs []string) (int, error) {
	dict, err := sign.ReadDetachedMetadata(r, commit)
	if err != nil {
		return 0, err
	}
	raw, err := sign.DetachedMetadataSignatures(dict, "ostree.gpgsigs")
	if err != nil {
		return 0, err
	}
	if raw == nil {
		return 0, nil
	}
	var kept [][]byte
	deleted := 0
	for _, sig := range raw {
		issuers, err := gpg.SignatureIssuerKeyIDs(sig)
		if err != nil {
			kept = append(kept, sig)
			continue
		}
		matched := false
		for _, issuer := range issuers {
			for _, keyID := range keyIDs {
				if gpg.KeyIDMatches(issuer, keyID) {
					matched = true
				}
			}
		}
		if matched {
			deleted++
			continue
		}
		kept = append(kept, sig)
	}
	if deleted == 0 {
		return 0, nil
	}
	if err := sign.SetDetachedSignatures(r, commit, dict, "ostree.gpgsigs", kept); err != nil {
		return 0, err
	}
	return deleted, nil
}
