package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/myml/ostree-go/pkg/bootconfig"
	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/pkg/pull"
	"github.com/myml/ostree-go/pkg/repo"
	"github.com/myml/ostree-go/pkg/sysroot"
	"github.com/myml/ostree-go/pkg/tree"
)

func adminDeployOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:       "os",
			ArgType:        cli.ArgString,
			ArgDescription: "OSNAME",
			Description:    "Use a different operating system root than the current one",
		},
		{
			LongName:       "stateroot",
			ArgType:        cli.ArgString,
			ArgDescription: "STATEROOT",
			Description:    "Target the provided stateroot",
		},
		{
			LongName:       "origin-file",
			ArgType:        cli.ArgFilename,
			ArgDescription: "FILENAME",
			Description:    "Specify origin file",
		},
		{
			LongName:    "no-prune",
			ArgType:     cli.ArgNone,
			Description: "Don't prune the repo when done",
		},
		{
			LongName:    "no-merge",
			ArgType:     cli.ArgNone,
			Description: "Do not apply configuration (/etc and kernel arguments) from booted deployment",
		},
		{
			LongName:    "retain",
			ArgType:     cli.ArgNone,
			Description: "Do not delete previous deployments",
		},
		{
			LongName:    "stage",
			ArgType:     cli.ArgNone,
			Description: "Complete deployment at OS shutdown",
		},
		{
			LongName:    "lock-finalization",
			ArgType:     cli.ArgNone,
			Description: "Prevent automatic deployment finalization on shutdown",
		},
		{
			LongName:    "retain-pending",
			ArgType:     cli.ArgNone,
			Description: "Do not delete pending deployments",
		},
		{
			LongName:    "retain-rollback",
			ArgType:     cli.ArgNone,
			Description: "Do not delete rollback deployments",
		},
		{
			LongName:    "not-as-default",
			ArgType:     cli.ArgNone,
			Description: "Append rather than prepend new deployment",
		},
		{
			LongName:    "karg-proc-cmdline",
			ArgType:     cli.ArgNone,
			Description: "Import current /proc/cmdline",
		},
		{
			LongName:       "karg",
			ArgType:        cli.ArgStringArray,
			ArgDescription: "NAME=VALUE",
			Description:    "Set kernel argument, like root=/dev/sda1; this overrides any earlier argument with the same name",
		},
		{
			LongName:       "karg-append",
			ArgType:        cli.ArgStringArray,
			ArgDescription: "NAME=VALUE",
			Description:    "Append kernel argument; useful with e.g. console= that can be used multiple times",
		},
		{
			LongName:    "karg-none",
			ArgType:     cli.ArgNone,
			Description: "Do not import kernel arguments",
		},
		{
			LongName:       "karg-delete",
			ArgType:        cli.ArgStringArray,
			ArgDescription: "NAME=VALUE",
			Description:    "Delete kernel argument if exists, can be used multiple times",
		},
		{
			LongName:       "overlay-initrd",
			ArgType:        cli.ArgStringArray,
			ArgDescription: "FILE",
			Description:    "Overlay iniramfs file",
		},
	}
}

func runAdminDeploy(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("REF/REV must be specified")
	}
	opts := ctx.Options
	if opts.Has("karg-proc-cmdline") && opts.Has("karg-none") {
		return fmt.Errorf("Can't specify both --karg-proc-cmdline and --karg-none")
	}
	if opts.Has("karg-none") && opts.Has("karg-delete") {
		return fmt.Errorf("Can't specify both --karg-none and --karg-delete")
	}
	if opts.Has("no-merge") && opts.Has("karg-delete") {
		return fmt.Errorf("Can't specify both --no-merge and --karg-delete")
	}
	stage := opts.Has("stage")
	// Locking implies staging.
	if opts.Has("lock-finalization") {
		stage = true
	}

	refspec := ctx.Args[0]
	osname := adminOsname(ctx)

	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}

	if s.Booted == nil && osname == "" {
		return fmt.Errorf("Looking for booted deployment: Not currently booted into an OSTree system and no --os= argument given")
	}

	var origin *repo.Config
	if originPath := opts.String("origin-file"); originPath != "" {
		data, err := os.ReadFile(originPath)
		if err != nil {
			return err
		}
		origin, err = repo.ParseConfig(data)
		if err != nil {
			return err
		}
	} else {
		origin = s.OriginNewFromRefspec(refspec)
	}

	revision, _, err := s.Repo.ResolveRef(refspec, false, true)
	if err != nil {
		return err
	}

	var merge *sysroot.Deployment
	if !opts.Has("no-merge") {
		merge = s.GetMergeDeployment(osname)
	}

	if err := s.PrepareCleanup(); err != nil {
		return fmt.Errorf("Performing initial cleanup: %w", err)
	}

	var kargs *bootconfig.KernelArgs
	switch {
	case opts.Has("karg-none"):
		kargs = bootconfig.NewKernelArgs()
	case opts.Has("karg-proc-cmdline"):
		kargs, err = procCmdlineKargs()
		if err != nil {
			return err
		}
	case merge != nil && (opts.Has("karg") || opts.Has("karg-append") || opts.Has("karg-delete")):
		if merge.BootConfig != nil {
			kargs = bootconfig.FromString(merge.BootConfig.Get("options"))
		}
	}
	if vals := opts.Strings("karg"); len(vals) > 0 {
		if kargs == nil {
			kargs = bootconfig.NewKernelArgs()
		}
		kargs.ReplaceArgv(vals)
	}
	if vals := opts.Strings("karg-append"); len(vals) > 0 {
		if kargs == nil {
			kargs = bootconfig.NewKernelArgs()
		}
		kargs.AppendArgv(vals)
	}
	for _, arg := range opts.Strings("karg-delete") {
		if kargs == nil {
			kargs = bootconfig.NewKernelArgs()
		}
		if err := kargs.DeleteIfPresent(arg); err != nil {
			return err
		}
	}

	deployOpts := &sysroot.DeployOptions{}
	for _, path := range opts.Strings("overlay-initrd") {
		csum, err := s.StageOverlayInitrd(path)
		if err != nil {
			return err
		}
		deployOpts.OverlayInitrds = append(deployOpts.OverlayInitrds, csum)
	}
	if kargs != nil {
		deployOpts.OverrideKernelArgs = kargs.Strv()
		deployOpts.OverrideKernelArgsSet = true
	}

	if stage {
		if opts.Has("retain-pending") || opts.Has("retain-rollback") {
			return fmt.Errorf("--stage cannot currently be combined with --retain arguments")
		}
		if opts.Has("not-as-default") {
			return fmt.Errorf("--stage cannot currently be combined with --not-as-default")
		}
		if s.Booted == nil {
			return fmt.Errorf("Staging deployment: Cannot stage deployment: Not currently booted into an OSTree system")
		}
		return fmt.Errorf("Staging deployments is not supported yet")
	}

	newDeployment, err := s.DeployTree(osname, revision, origin, merge, deployOpts)
	if err != nil {
		return fmt.Errorf("Deploying tree: %w", err)
	}

	flags := sysroot.SimpleWriteNoClean
	if opts.Has("retain") {
		flags |= sysroot.SimpleWriteRetain
	} else {
		if opts.Has("retain-pending") {
			flags |= sysroot.SimpleWriteRetainPending
		}
		if opts.Has("retain-rollback") {
			flags |= sysroot.SimpleWriteRetainRollback
		}
	}
	if opts.Has("not-as-default") {
		flags |= sysroot.SimpleWriteNotDefault
	}
	if err := s.SimpleWriteDeployment(osname, newDeployment, merge, flags); err != nil {
		return err
	}

	if opts.Has("no-prune") {
		return s.PrepareCleanup()
	}
	return s.Cleanup(true)
}

func adminSwitchOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:    "reboot",
			ShortName:   'r',
			ArgType:     cli.ArgNone,
			Description: "Reboot after switching trees",
		},
		{
			LongName:    "kexec",
			ShortName:   'k',
			ArgType:     cli.ArgNone,
			Description: "Stage new kernel in kexec",
		},
		{
			LongName:       "os",
			ArgType:        cli.ArgString,
			ArgDescription: "OSNAME",
			Description:    "Use a different operating system root than the current one",
		},
	}
}

func adminUpgradeOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:       "os",
			ArgType:        cli.ArgString,
			ArgDescription: "OSNAME",
			Description:    "Use a different operating system root than the current one",
		},
		{
			LongName:    "reboot",
			ShortName:   'r',
			ArgType:     cli.ArgNone,
			Description: "Reboot after a successful upgrade",
		},
		{
			LongName:    "kexec",
			ShortName:   'k',
			ArgType:     cli.ArgNone,
			Description: "Stage new kernel in kexec",
		},
		{
			LongName:    "allow-downgrade",
			ArgType:     cli.ArgNone,
			Description: "Permit deployment of chronologically older trees",
		},
		{
			LongName:       "override-commit",
			ArgType:        cli.ArgString,
			ArgDescription: "CHECKSUM",
			Description:    "Deploy CHECKSUM instead of the latest tree",
		},
		{
			LongName:    "pull-only",
			ArgType:     cli.ArgNone,
			Description: "Do not create a deployment, just download",
		},
		{
			LongName:    "deploy-only",
			ArgType:     cli.ArgNone,
			Description: "Do not pull, only deploy",
		},
		{
			LongName:    "stage",
			ArgType:     cli.ArgNone,
			Description: "Enable staging (finalization at reboot time)",
		},
	}
}

// upgrader mirrors OstreeSysrootUpgrader.
type upgrader struct {
	s                  *sysroot.Sysroot
	osname             string
	ignoreUnconfigured bool
	merge              *sysroot.Deployment
	origin             *repo.Config
	originRemote       string
	originRef          string
	overrideCsum       string
	newRevision        string
}

// newUpgrader implements ostree_sysroot_upgrader_new_for_os_with_flags().
func newUpgrader(s *sysroot.Sysroot, osname string, ignoreUnconfigured bool) (*upgrader, error) {
	if s.Booted == nil && osname == "" {
		return nil, fmt.Errorf("Not currently booted into an OSTree system and no OS specified")
	}
	if osname == "" {
		osname = s.Booted.OsName
	} else if osname == "" {
		return nil, fmt.Errorf("Invalid empty osname")
	}
	merge := s.GetMergeDeployment(osname)
	if merge == nil {
		return nil, fmt.Errorf("No previous deployment for OS '%s'", osname)
	}
	if merge.Origin == nil {
		return nil, fmt.Errorf("No origin known for deployment %s.%d", merge.Checksum, merge.DeploySerial)
	}
	u := &upgrader{
		s:                  s,
		osname:             osname,
		ignoreUnconfigured: ignoreUnconfigured,
		merge:              merge,
		origin:             merge.Origin.Clone(),
	}
	if err := u.parseRefspec(); err != nil {
		return nil, err
	}
	return u, nil
}

// parseRefspec implements the upgrader's parse_refspec().
func (u *upgrader) parseRefspec() error {
	if !u.ignoreUnconfigured {
		if v, err := u.origin.Get("origin", "unconfigured-state"); err == nil && v != "" {
			return fmt.Errorf("origin unconfigured-state: %s", v)
		}
	}
	refspec, err := u.origin.Get("origin", "refspec")
	if err != nil || refspec == "" {
		return fmt.Errorf("No origin/refspec in current deployment origin; cannot upgrade via ostree")
	}
	remote, name, err := repo.ParseRefSpec(refspec)
	if err != nil {
		return err
	}
	u.originRemote = remote
	u.originRef = name
	u.overrideCsum = ""
	if csum, err := u.origin.Get("origin", "override-commit"); err == nil && csum != "" {
		if err := repo.ValidateChecksum(csum); err != nil {
			return err
		}
		u.overrideCsum = csum
	}
	return nil
}

// setOrigin replaces the upgrader's origin.
func (u *upgrader) setOrigin(cfg *repo.Config) error {
	u.origin = cfg
	return u.parseRefspec()
}

// refspec returns the origin refspec (remote:ref or ref).
func (u *upgrader) refspec() string {
	if u.originRemote != "" {
		return u.originRemote + ":" + u.originRef
	}
	return u.originRef
}

// pull implements ostree_sysroot_upgrader_pull().
func (u *upgrader) pull(allowOlder, synthetic bool) (bool, error) {
	fromRevision := u.merge.Checksum

	if u.originRemote != "" && !synthetic {
		if u.overrideCsum == "" {
			// libostree passes timestamp-check-from-rev to the pull so that a
			// chronologically older commit is rejected *before* any object or
			// ref is written.  Resolve the remote's target first (a dry-run
			// pull writes nothing) and compare timestamps.
			if fromRevision != "" && !allowOlder {
				if err := u.checkTimestampsBeforePull(fromRevision); err != nil {
					return false, err
				}
			}
			if _, err := pull.Remote(pull.RemoteOptions{
				Repo:   u.s.Repo,
				Remote: u.originRemote,
				Refs:   []string{u.originRef},
			}); err != nil {
				return false, err
			}
		} else {
			has, err := u.s.Repo.HasObject(u.overrideCsum, repo.ObjectCommit)
			if err != nil {
				return false, err
			}
			if !has {
				return false, fmt.Errorf("Commit %s not found in repository", u.overrideCsum)
			}
		}
	}

	originRefspec := u.refspec()
	newRevision, _, err := u.s.Repo.ResolveRef(originRefspec, false, true)
	if err != nil {
		return false, err
	}

	// Check to see if the commit marks the ref as EOL, redirecting to another.
	if commit, err := tree.LoadCommit(u.s.Repo, newRevision); err == nil {
		if rebase, ok := commit.GetMetadataString("ostree.endoflife-rebase"); ok && rebase != "" {
			if u.originRemote != "" && !synthetic {
				if _, err := pull.Remote(pull.RemoteOptions{
					Repo:   u.s.Repo,
					Remote: u.originRemote,
					Refs:   []string{rebase},
				}); err != nil {
					return false, err
				}
			}
			u.originRef = rebase
			u.origin.Set("origin", "refspec", u.refspec())
			if u.overrideCsum == "" {
				originRefspec = u.refspec()
				newRevision, _, err = u.s.Repo.ResolveRef(originRefspec, false, true)
				if err != nil {
					return false, err
				}
			}
		}
	}

	if u.overrideCsum != "" {
		if u.originRemote != "" {
			if err := u.s.Repo.WriteRemoteRef(u.originRemote, u.originRef, u.overrideCsum); err != nil {
				return false, err
			}
		} else if err := u.s.Repo.WriteRef(u.originRef, u.overrideCsum); err != nil {
			return false, err
		}
		u.newRevision = u.overrideCsum
	} else {
		u.newRevision = newRevision
	}

	if fromRevision == u.newRevision {
		return false, nil
	}
	if fromRevision != "" && !allowOlder {
		if err := checkTimestamps(u.s.Repo, fromRevision, u.newRevision); err != nil {
			return false, err
		}
	}
	return true, nil
}

// checkTimestampsBeforePull resolves the remote's current target for the
// origin ref without downloading it and rejects a downgrade early.
func (u *upgrader) checkTimestampsBeforePull(fromRevision string) error {
	res, err := pull.Remote(pull.RemoteOptions{
		Repo:   u.s.Repo,
		Remote: u.originRemote,
		Refs:   []string{u.originRef},
		DryRun: true,
	})
	if err != nil {
		// Fall back to the post-pull check (which still runs below).
		return nil
	}
	target := res.Refs[u.originRef]
	if target == "" || target == fromRevision {
		return nil
	}
	oldCommit, err := tree.LoadCommit(u.s.Repo, fromRevision)
	if err != nil {
		return nil
	}
	if newCommit, err := tree.LoadCommit(u.s.Repo, target); err == nil {
		return compareTimestamps(fromRevision, oldCommit.Timestamp, target, newCommit.Timestamp)
	}
	url, err := u.s.Repo.Config().GetRemote(u.originRemote, "url")
	if err != nil || url == "" {
		return nil
	}
	ts, err := pull.FetchCommitTimestamp(url, target)
	if err != nil {
		return nil
	}
	return compareTimestamps(fromRevision, oldCommit.Timestamp, target, ts)
}

// compareTimestamps mirrors _ostree_compare_timestamps().
func compareTimestamps(currentRev string, currentTS uint64, newRev string, newTS uint64) error {
	if newTS > currentTS {
		return nil
	}
	if newTS == currentTS || currentRev == newRev {
		return nil
	}
	format := func(ts uint64) string {
		return time.Unix(int64(ts), 0).UTC().Format("Mon Jan _2 15:04:05 2006")
	}
	return fmt.Errorf(
		"Upgrade target revision '%s' with timestamp '%s' is chronologically older than current revision '%s' with timestamp '%s'; use --allow-downgrade to permit",
		newRev, format(newTS), currentRev, format(currentTS))
}

// checkTimestamps implements ostree_sysroot_upgrader_check_timestamps().
func checkTimestamps(r *repo.Repo, fromRev, toRev string) error {
	oldCommit, err := tree.LoadCommit(r, fromRev)
	if err != nil {
		return err
	}
	newCommit, err := tree.LoadCommit(r, toRev)
	if err != nil {
		return err
	}
	return compareTimestamps(fromRev, oldCommit.Timestamp, toRev, newCommit.Timestamp)
}

// deploy implements ostree_sysroot_upgrader_deploy().
func (u *upgrader) deploy(stage bool) error {
	if stage || os.Getenv("OSTREE_EX_STAGE_DEPLOYMENTS") != "" {
		if u.s.Booted == nil {
			return fmt.Errorf("Staging deployment: Cannot stage deployment: Not currently booted into an OSTree system")
		}
		return fmt.Errorf("Staging deployments is not supported yet")
	}
	newDeployment, err := u.s.DeployTree(u.osname, u.newRevision, u.origin, u.merge,
		&sysroot.DeployOptions{})
	if err != nil {
		return fmt.Errorf("Deploying tree: %w", err)
	}
	return u.s.SimpleWriteDeployment(u.osname, newDeployment, u.merge, 0)
}

func runAdminUpgrade(ctx *cli.Context) error {
	opts := ctx.Options
	if opts.Has("pull-only") && opts.Has("deploy-only") {
		return fmt.Errorf("Cannot simultaneously specify --pull-only and --deploy-only")
	}
	if opts.Has("pull-only") && (opts.Has("reboot") || opts.Has("kexec")) {
		return fmt.Errorf("Cannot simultaneously specify --pull-only and --reboot or --kexec")
	}
	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}
	u, err := newUpgrader(s, adminOsname(ctx), false)
	if err != nil {
		return err
	}
	// Drop transient state (pinned/unlocked/override-commit) before pulling,
	// then re-add the explicit override if one was requested.
	if u.origin.HasGroup(sysroot.OriginTransientGroup) {
		if err := u.origin.RemoveGroup(sysroot.OriginTransientGroup); err != nil {
			return err
		}
	}
	_ = u.origin.Unset("origin", "override-commit")
	_ = u.origin.Unset("origin", "unlocked")
	if override := opts.String("override-commit"); override != "" {
		if err := repo.ValidateChecksum(override); err != nil {
			return err
		}
		u.origin.Set("origin", "override-commit", override)
	}
	if err := u.setOrigin(u.origin); err != nil {
		return err
	}

	changed, err := u.pull(opts.Has("allow-downgrade"), opts.Has("deploy-only"))
	if err != nil {
		if opts.Has("pull-only") {
			_ = s.Cleanup(true)
		}
		return err
	}

	if !changed {
		fmt.Fprintln(ctx.Stdout, "No update available.")
		return nil
	}
	if opts.Has("pull-only") {
		return nil
	}
	if err := u.deploy(opts.Has("stage")); err != nil {
		return err
	}
	if opts.Has("reboot") {
		return fmt.Errorf("Rebooting is not supported yet")
	}
	return nil
}

func runAdminSwitch(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("REFSPEC must be specified")
	}
	newProvidedRefspec := ctx.Args[0]
	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}
	u, err := newUpgrader(s, adminOsname(ctx), true)
	if err != nil {
		return err
	}
	oldRemote, oldRef := u.originRemote, u.originRef

	newRemote, newRef := "", ""
	if strings.HasSuffix(newProvidedRefspec, ":") {
		newRemote = strings.TrimSuffix(newProvidedRefspec, ":")
		newRef = oldRef
	} else {
		newRemote, newRef, err = repo.ParseRefSpec(newProvidedRefspec)
		if err != nil {
			return err
		}
	}
	remote := newRemote
	if remote == "" {
		remote = oldRemote
	}
	newRefspec := newRef
	if remote != "" {
		newRefspec = remote + ":" + newRef
	}
	if originRefspec := u.refspec(); originRefspec == newRefspec {
		return fmt.Errorf("Old and new refs are equal: %s", newRefspec)
	}

	newOrigin := s.OriginNewFromRefspec(newRefspec)
	if err := u.setOrigin(newOrigin); err != nil {
		return err
	}

	if _, err := u.pull(true, false); err != nil {
		return err
	}
	if err := u.deploy(false); err != nil {
		return err
	}

	// Delete the old ref now that the new one is deployed.
	fmt.Fprintf(ctx.Stdout, "Deleting ref '%s:%s'\n", oldRemote, oldRef)
	if oldRemote != "" {
		_ = s.Repo.DeleteRefSpec(oldRemote + ":" + oldRef)
	} else {
		_ = s.Repo.DeleteRef(oldRef)
	}
	if ctx.Options.Has("reboot") {
		return fmt.Errorf("Rebooting is not supported yet")
	}
	return nil
}

func adminSetOriginOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:       "set",
			ShortName:      's',
			ArgType:        cli.ArgStringArray,
			ArgDescription: "KEY=VALUE",
			Description:    "Set config option KEY=VALUE for remote",
		},
		{
			LongName:       "index",
			ArgType:        cli.ArgInt,
			ArgDescription: "INDEX",
			Description:    "Operate on the deployment INDEX, starting from zero",
		},
	}
}

func runAdminSetOrigin(ctx *cli.Context) error {
	if len(ctx.Args) < 2 {
		return fmt.Errorf("REMOTENAME and URL must be specified")
	}
	remotename := ctx.Args[0]
	url := ctx.Args[1]
	branch := ""
	if len(ctx.Args) > 2 {
		branch = ctx.Args[2]
	}
	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}

	// ostree_repo_remote_change(ADD_IF_NOT_EXISTS)
	cfg := s.Repo.Config()
	if !cfg.HasGroup(repo.RemoteSection(remotename)) {
		cfg.SetRemote(remotename, "url", url)
		for _, kv := range ctx.Options.Strings("set") {
			i := strings.IndexByte(kv, '=')
			if i <= 0 {
				return fmt.Errorf("Invalid key/value pair %q", kv)
			}
			cfg.SetRemote(remotename, kv[:i], kv[i+1:])
		}
		if err := saveConfig(s.Repo, cfg); err != nil {
			return err
		}
	}

	var target *sysroot.Deployment
	if ctx.Options.Has("index") {
		target, err = s.GetIndexedDeployment(ctx.Options.Int("index"))
		if err != nil {
			return err
		}
	} else {
		target, err = s.RequireBootedDeployment()
		if err != nil {
			return err
		}
	}

	originRefspec := target.OriginRefspec()
	var originRef string
	if originRefspec != "" {
		_, originRef, err = repo.ParseRefSpec(originRefspec)
		if err != nil {
			return err
		}
	} else if branch == "" {
		return fmt.Errorf("No host refspec found, branch is required")
	}
	targetBranch := branch
	if targetBranch == "" {
		targetBranch = originRef
	}
	newOrigin := s.OriginNewFromRefspec(remotename + ":" + targetBranch)
	return s.WriteOriginFileAndBump(target, newOrigin)
}
