// Package cmd wires the concrete OSTree commands into the CLI framework.
package cmd

import (
	"errors"
	"fmt"

	"ostree-go/internal/cli"
	"ostree-go/internal/repo"
)

// Setup registers the implemented commands and the repository opener on app.
func Setup(app *cli.App) {
	// Only advertise features that are actually implemented; upstream
	// tests use this list to decide what to skip.
	app.Features = []string{"initial-var", "libarchive", "sign-ed25519"}
	app.OpenRepo = openRepo
	for _, c := range app.Commands {
		switch c.Name {
		case "admin":
			registerAdmin(c)
		case "refs":
			c.Run = runRefs
		case "rev-parse":
			c.Run = runRevParse
		case "checkout":
			c.Options = checkoutOptions()
			c.Run = runCheckout
		case "cat":
			c.Run = runCat
		case "summary":
			c.Options = summaryOptions()
			c.Run = runSummary
		case "init":
			c.Options = initOptions()
			c.Run = runInit
		case "ls":
			c.Options = lsOptions()
			c.Run = runLs
		case "log":
			c.Options = logOptions()
			c.Run = runLog
		case "show":
			c.Options = showOptions()
			c.Run = runShow
		case "static-delta":
			registerStaticDelta(c)
		case "fsck":
			c.Options = fsckOptions()
			c.Run = runFsck
		case "export":
			c.Options = exportOptions()
			c.Run = runExport
		case "diff":
			c.Options = diffOptions()
			c.Run = runDiff
		case "pull":
			c.Options = pullOptions()
			c.Run = runPull
		case "find-remotes":
			c.Options = findRemotesOptions()
			c.Run = runFindRemotes
		case "reset":
			c.Run = runReset
		case "checksum":
			c.Options = checksumOptions()
			c.Run = runChecksum
		case "commit":
			c.Options = commitOptions()
			c.Run = runCommit
		case "gpg-sign":
			c.Options = gpgSignOptions()
			c.Run = runGpgSign
		case "sign":
			c.Options = signOptions()
			c.Run = runSign
		case "config":
			c.Options = configOptions()
			c.Run = runConfig
		case "pull-local":
			c.Options = pullLocalOptions()
			c.Run = runPullLocal
		case "prune":
			c.Options = pruneOptions()
			c.Run = runPrune
		case "remote":
			for _, s := range c.Subcommands {
				switch s.Name {
				case "add":
					s.Options = append(remoteAddOptions(), remoteAddExtraOptions()...)
					s.Run = runRemoteAddWithExtras
				case "delete":
					s.Options = remoteDeleteOptions()
					s.Run = runRemoteDeleteWithKeyring
				case "gpg-import":
					s.Options = remoteGpgImportOptions()
					s.Run = runRemoteGpgImport
				case "gpg-list-keys":
					s.Options = remoteGpgListKeysOptions()
					s.Run = runRemoteGpgListKeys
				case "list":
					s.Options = remoteListOptions()
					s.Run = runRemoteList
				case "show-url":
					s.Run = runRemoteShowURL
				case "add-cookie":
					s.Run = runRemoteAddCookie
				case "delete-cookie":
					s.Run = runRemoteDeleteCookie
				case "list-cookies":
					s.Run = runRemoteListCookies
				case "refs":
					s.Options = remoteRefsOptions()
					s.Run = runRemoteRefs
				case "summary":
					s.Options = remoteSummaryOptions()
					s.Run = runRemoteSummaryDump
				}
			}
		}
	}
}

func openRepo(path string) (cli.Repo, error) {
	r, err := repo.Open(path)
	if err != nil {
		if errors.Is(err, repo.ErrNotARepo) {
			return nil, &cli.RepoOpenError{
				Message:  fmt.Sprintf("opening repo: %v", err),
				NotFound: true,
			}
		}
		return nil, fmt.Errorf("opening repo: %w", err)
	}
	return r, nil
}

func repoFrom(ctx *cli.Context) (*repo.Repo, error) {
	r, ok := ctx.Repo.(*repo.Repo)
	if !ok || r == nil {
		return nil, fmt.Errorf("no repository")
	}
	return r, nil
}
