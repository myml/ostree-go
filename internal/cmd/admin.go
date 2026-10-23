package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ostree-go/internal/bootconfig"
	"ostree-go/internal/cli"
	"ostree-go/internal/gpg"
	"ostree-go/internal/repo"
	"ostree-go/internal/sign"
	"ostree-go/internal/sysroot"
)

// adminSubcommandHelp is the static subcommand table used when `ostree admin`
// is invoked without a subcommand.  The column width matches the C
// ot-builtin-admin.c (25).
var adminSubcommandHelp = []struct {
	name, description string
	hidden            bool
}{
	{"cleanup", "Delete untagged deployments and repository objects", false},
	{"config-diff", "Diff current /etc configuration versus default", false},
	{"deploy", "Checkout revision REFSPEC as the new default deployment", false},
	{"finalize-staged", "Internal command to run at shutdown time", true},
	{"lock-finalization", "Change the finalization locking state of the staged deployment", false},
	{"boot-complete", "Internal command to run at boot after an update was applied", true},
	{"state-overlay", "Internal command to assemble a state overlay", true},
	{"init-fs", "Initialize a root filesystem", false},
	{"instutil", "Deprecated commands intended for installer programs", true},
	{"os-init", "Initialize empty state for given operating system", false},
	{"stateroot-init", "Initialize empty state for given operating system", false},
	{"pin", `Change the "pinning" state of a deployment, INDEX can be >= 0 or one of booted, pending or rollback strings`, false},
	{"post-copy", "Update the repo and deployments as needed after a copy", false},
	{"set-origin", "Set Origin and create a new origin file", false},
	{"status", "List deployments", false},
	{"switch", "Construct new tree from REFSPEC and deploy it", false},
	{"undeploy", "Delete deployment INDEX", false},
	{"set-default", "Make deployment INDEX the default", false},
	{"unlock", "Make the current deployment mutable (as a hotfix or development)", false},
	{"upgrade", "Construct new tree from current origin and deploy it, if it changed", false},
	{"kargs", "Change kernel arguments", false},
}

// writeAdminHelp renders a minimal GOption-compatible help page.
func writeAdminHelp(w io.Writer) {
	fmt.Fprintf(w, "Usage:\n  ostree admin [OPTION\u2026] --print-current-dir|COMMAND\n\n")
	fmt.Fprintf(w, "Builtin \"admin\" Commands:\n")
	for _, sub := range adminSubcommandHelp {
		if sub.hidden {
			continue
		}
		fmt.Fprintf(w, "  %-25s%s\n", sub.name, sub.description)
	}
	fmt.Fprintf(w, "\nHelp Options:\n  -h, --help                Show help options\n")
	fmt.Fprintf(w, "\nApplication Options:\n")
	fmt.Fprintf(w, "  --sysroot=PATH            Create a new OSTree sysroot at PATH\n")
	fmt.Fprintf(w, "  -v, --verbose             Print debug information during command processing\n")
	fmt.Fprintf(w, "  --version                 Print version information and exit\n")
}

// runAdminRoot handles `ostree admin` without a subcommand: either
// --print-current-dir or the missing-subcommand error.
func runAdminRoot(ctx *cli.Context) error {
	writeAdminHelp(ctx.Stderr)
	return fmt.Errorf(`No "admin" subcommand specified`)
}

// runAdminPrintCurrentDir implements the hidden --print-current-dir admin
// option: print the first deployment's directory and exit successfully.  It is
// accepted by every admin subcommand.
func runAdminPrintCurrentDir(ctx *cli.Context) error {
	s, err := openSysroot(ctx)
	if err != nil {
		return err
	}
	if len(s.Deployments) == 0 {
		return fmt.Errorf("Unable to find a deployment in sysroot")
	}
	fmt.Fprintln(ctx.Stdout, s.Deployments[0].DirPath(s.Path))
	return nil
}

// withAdminGlobals wraps an admin handler with the behavior shared by all
// admin builtins (--print-current-dir).
func withAdminGlobals(run func(*cli.Context) error) func(*cli.Context) error {
	return func(ctx *cli.Context) error {
		if ctx.PrintCurrentDir {
			return runAdminPrintCurrentDir(ctx)
		}
		return run(ctx)
	}
}

// registerAdmin wires every implemented admin subcommand.
func registerAdmin(c *cli.Command) {
	c.Run = withAdminGlobals(runAdminRoot)
	for _, sub := range c.Subcommands {
		opts, run := adminSubcommand(sub.Name)
		if run == nil {
			continue
		}
		sub.Options = opts
		sub.Run = withAdminGlobals(run)
	}
}

// adminSubcommand returns the option table and handler for one admin
// subcommand, or a nil handler when it is not implemented yet.
func adminSubcommand(name string) ([]cli.Option, func(*cli.Context) error) {
	switch name {
	case "init-fs":
		return initFsOptions(), runAdminInitFs
	case "os-init", "stateroot-init":
		return nil, runAdminOsInit
	case "deploy":
		return adminDeployOptions(), runAdminDeploy
	case "status":
		return adminStatusOptions(), runAdminStatus
	case "switch":
		return adminSwitchOptions(), runAdminSwitch
	case "upgrade":
		return adminUpgradeOptions(), runAdminUpgrade
	case "cleanup":
		return nil, runAdminCleanup
	case "set-default":
		return nil, runAdminSetDefault
	case "undeploy":
		return nil, runAdminUndeploy
	case "pin":
		return adminPinOptions(), runAdminPin
	case "set-origin":
		return adminSetOriginOptions(), runAdminSetOrigin
	case "kargs":
		return adminKargsOptions(), runAdminKargs
	case "instutil":
		return adminInstutilOptions(), runAdminInstutil
	case "unlock":
		return adminUnlockOptions(), runAdminUnlock
	case "lock-finalization":
		return adminLockFinalizationOptions(), runAdminLockFinalization
	case "finalize-staged":
		return nil, runAdminFinalizeStaged
	case "post-copy":
		return nil, runAdminPostCopy
	case "state-overlay":
		return nil, runAdminStateOverlay
	}
	return nil, nil
}

// adminSysrootPath resolves --sysroot / $OSTREE_SYSROOT / "/".
func adminSysrootPath(ctx *cli.Context) string {
	if ctx.Options.Has("sysroot") {
		return ctx.Sysroot
	}
	return sysroot.DefaultPath()
}

// openSysroot opens (and loads) the sysroot selected by the admin options.
func openSysroot(ctx *cli.Context) (*sysroot.Sysroot, error) {
	s, err := sysroot.Open(adminSysrootPath(ctx))
	if err != nil {
		return nil, err
	}
	s.Err = ctx.Stdout
	s.Stderr = ctx.Stderr
	return s, nil
}

// openSysrootLocked opens the sysroot while holding its write lock, which
// every mutating admin builtin does (OSTREE_ADMIN_BUILTIN_FLAG_SUPERUSER).
func openSysrootLocked(ctx *cli.Context) (*sysroot.Sysroot, error) {
	s, unlock, err := sysroot.OpenLocked(adminSysrootPath(ctx))
	if err != nil {
		return nil, err
	}
	// The lock is held until the process exits.
	_ = unlock
	s.Err = ctx.Stdout
	s.Stderr = ctx.Stderr
	return s, nil
}

// adminOsname returns --os/--stateroot.
func adminOsname(ctx *cli.Context) string {
	if v := ctx.Options.String("os"); v != "" {
		return v
	}
	return ctx.Options.String("stateroot")
}

// atoi mimics C atoi(): parse the leading digits, default to 0.
func atoi(s string) int {
	end := 0
	for end < len(s) && (s[end] == '-' || s[end] == '+' || (s[end] >= '0' && s[end] <= '9')) {
		if end > 0 && (s[end] == '-' || s[end] == '+') {
			break
		}
		end++
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0
	}
	return n
}

// procCmdlineKargs imports /proc/cmdline, dropping BOOT_IMAGE= and initrd=.
func procCmdlineKargs() (*bootconfig.KernelArgs, error) {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, err
	}
	kargs := bootconfig.NewKernelArgs()
	for _, arg := range bootconfig.SplitKernelArgs(strings.TrimRight(string(data), "\n")) {
		if strings.HasPrefix(arg, "BOOT_IMAGE=") || strings.HasPrefix(arg, "initrd=") {
			continue
		}
		kargs.Append(arg)
	}
	return kargs, nil
}

// initFsOptions mirrors ot-admin-builtin-init-fs.c.
func initFsOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:    "modern",
			ArgType:     cli.ArgNone,
			Description: "Only create /boot and /ostree",
		},
		{
			LongName:       "epoch",
			ShortName:      'E',
			ArgType:        cli.ArgInt,
			ArgDescription: "N",
			Description:    "An integer value, defines initial state.  Must be in the range [0-1], inclusive.",
		},
	}
}

func runAdminInitFs(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("PATH must be specified")
	}
	return sysroot.InitFs(ctx.Args[0], sysroot.InitFsOptions{
		Modern: ctx.Options.Has("modern"),
		Epoch:  ctx.Options.Int("epoch"),
	})
}

func runAdminOsInit(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("STATEROOT must be specified")
	}
	path, err := filepath.Abs(adminSysrootPath(ctx))
	if err != nil {
		return err
	}
	s := &sysroot.Sysroot{Path: path}
	if err := s.EnsureInitialized(); err != nil {
		return err
	}
	osname := ctx.Args[0]
	if err := s.InitOsName(osname); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "ostree/deploy/%s initialized as OSTree stateroot\n", osname)
	return nil
}

func adminStatusOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:    "verify",
			ShortName:   'V',
			ArgType:     cli.ArgNone,
			Description: "Print the commit verification status",
		},
		{
			LongName:    "json",
			ShortName:   'J',
			ArgType:     cli.ArgNone,
			Description: "Emit JSON",
		},
		{
			LongName:    "skip-signatures",
			ShortName:   'S',
			ArgType:     cli.ArgNone,
			Description: "Skip signatures in output",
		},
		{
			LongName:    "is-default",
			ShortName:   'D',
			ArgType:     cli.ArgNone,
			Description: `Output "default" if booted into the default deployment, otherwise "not-default"`,
		},
	}
}

func jsonEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func runAdminStatus(ctx *cli.Context) error {
	s, err := openSysroot(ctx)
	if err != nil {
		return err
	}
	var pending, rollback *sysroot.Deployment
	if s.Booted != nil {
		pending, rollback = s.QueryDeploymentsFor("")
	}
	isBooted := func(d *sysroot.Deployment) bool { return d.Equal(s.Booted) }

	if ctx.Options.Has("json") {
		w := ctx.Stdout
		fmt.Fprintf(w, "{\n    \"deployments\": [")
		for i, d := range s.Deployments {
			if i > 0 {
				fmt.Fprintf(w, ",")
			}
			version := s.CommitVersion(d.Checksum)
			sourceTitle := s.CommitSourceTitle(d.Checksum)
			unlocked := d.UnlockedState
			if unlocked == "" {
				unlocked = sysroot.UnlockedNone
			}
			fmt.Fprintf(w, "\n        {\n")
			fmt.Fprintf(w, "            \"checksum\": \"%s\",\n", d.Checksum)
			fmt.Fprintf(w, "            \"stateroot\": \"%s\",\n", jsonEscape(d.OsName))
			if refspec := d.OriginRefspec(); refspec != "" {
				fmt.Fprintf(w, "            \"refspec\": \"%s\",\n", jsonEscape(refspec))
			} else {
				fmt.Fprintf(w, "            \"refspec\": null,\n")
			}
			fmt.Fprintf(w, "            \"serial\": %d,\n", d.DeploySerial)
			fmt.Fprintf(w, "            \"index\": %d,\n", d.Index)
			fmt.Fprintf(w, "            \"booted\": %t,\n", isBooted(d))
			fmt.Fprintf(w, "            \"pending\": %t,\n", d.Equal(pending))
			fmt.Fprintf(w, "            \"rollback\": %t,\n", d.Equal(rollback))
			fmt.Fprintf(w, "            \"finalization-locked\": %t,\n", false)
			fmt.Fprintf(w, "            \"soft-reboot-target\": %t,\n", false)
			fmt.Fprintf(w, "            \"staged\": %t,\n", d.Staged)
			fmt.Fprintf(w, "            \"pinned\": %t,\n", d.IsPinned())
			fmt.Fprintf(w, "            \"unlocked\": \"%s\"", unlocked)
			if version != "" {
				fmt.Fprintf(w, ",\n            \"version\": \"%s\"", jsonEscape(version))
			}
			if sourceTitle != "" {
				fmt.Fprintf(w, ",\n            \"source-title\": \"%s\"", jsonEscape(sourceTitle))
			}
			fmt.Fprintf(w, "\n        }")
		}
		if len(s.Deployments) > 0 {
			fmt.Fprintf(w, "\n    ")
		}
		fmt.Fprintf(w, "]\n}\n")
		return nil
	}

	if ctx.Options.Has("is-default") {
		if len(s.Deployments) == 0 {
			return fmt.Errorf("Not in a booted OSTree system")
		}
		if isBooted(s.Deployments[0]) {
			fmt.Fprintln(ctx.Stdout, "default")
		} else {
			fmt.Fprintln(ctx.Stdout, "not-default")
		}
		return nil
	}
	if len(s.Deployments) == 0 {
		fmt.Fprintln(ctx.Stdout, "No deployments.")
		return nil
	}
	for _, d := range s.Deployments {
		suffix := ""
		switch {
		case d.Staged:
			suffix += " (staged)"
		case d.Equal(pending):
			suffix += " (pending)"
		case d.Equal(rollback):
			suffix += " (rollback)"
		}
		marker := byte(' ')
		if isBooted(d) {
			marker = '*'
		}
		fmt.Fprintf(ctx.Stdout, "%c %s %s.%d%s\n", marker, d.OsName, d.Checksum, d.DeploySerial, suffix)
		if version := s.CommitVersion(d.Checksum); version != "" {
			fmt.Fprintf(ctx.Stdout, "    Version: %s\n", version)
		}
		if d.UnlockedState != "" && d.UnlockedState != sysroot.UnlockedNone {
			fmt.Fprintf(ctx.Stdout, "    Unlocked: %s\n", sysroot.UnlockedStateString(d.UnlockedState))
		}
		if d.IsPinned() {
			fmt.Fprintf(ctx.Stdout, "    Pinned: yes\n")
		}
		if d.Origin == nil {
			fmt.Fprintf(ctx.Stdout, "    origin: none\n")
		} else {
			refspec := d.OriginRefspec()
			if refspec == "" {
				fmt.Fprintf(ctx.Stdout, "    origin: <unknown origin type>\n")
			} else {
				fmt.Fprintf(ctx.Stdout, "    origin refspec: %s\n", refspec)
			}
			if sourceTitle := s.CommitSourceTitle(d.Checksum); sourceTitle != "" {
				fmt.Fprintf(ctx.Stdout, "    `- %s\n", sourceTitle)
			}
		}
		if !ctx.Options.Has("skip-signatures") {
			printDeploymentSignatures(ctx, s, d)
		}
	}
	return nil
}

// remoteGPGVerify reports whether the remote named by refspec verifies GPG
// signatures (ostree_repo_remote_get_gpg_verify).
func remoteGPGVerify(s *sysroot.Sysroot, remote string) bool {
	if remote == "" {
		return false
	}
	cfg := s.Repo.Config()
	if !cfg.HasGroup(repo.RemoteSection(remote)) {
		return false
	}
	v, err := cfg.GetRemote(remote, "gpg-verify")
	if err != nil || v == "" {
		return true
	}
	return v != "false" && v != "0"
}

// printDeploymentSignatures prints the GPG signatures of a deployment's commit
// like deployment_print_status() does.
func printDeploymentSignatures(ctx *cli.Context, s *sysroot.Sysroot, d *sysroot.Deployment) {
	refspec := d.OriginRefspec()
	if refspec == "" {
		return
	}
	remote, _, err := repo.ParseRefSpec(refspec)
	if err != nil || !remoteGPGVerify(s, remote) {
		return
	}
	commitData, err := s.Repo.ReadObject(d.Checksum, repo.ObjectCommit)
	if err != nil {
		return
	}
	dict, err := sign.ReadDetachedMetadata(s.Repo, d.Checksum)
	if err != nil || dict == nil {
		return
	}
	sigs, err := sign.DetachedMetadataSignatures(dict, sign.GpgSignaturesKey)
	if err != nil || len(sigs) == 0 {
		return
	}
	var signatureBytes []byte
	for _, sig := range sigs {
		signatureBytes = append(signatureBytes, sig...)
	}
	keyrings := gpg.KeyringFilesForRemote(s.Repo.Path(), remote)
	result, err := gpg.VerifyDetached(commitData, signatureBytes, keyrings)
	if err != nil || result == nil {
		// A missing public key means the commit is simply not signed by a
		// trusted key; libostree prints nothing in that case (status).
		return
	}
	keys := result.KeyIDs
	if len(keys) == 0 {
		keys = []string{""}
	}
	for _, keyID := range keys {
		fmt.Fprintf(ctx.Stdout, "    GPG: Signature made using key ID %s\n", keyID)
	}
	for _, uid := range result.UIDs {
		fmt.Fprintf(ctx.Stdout, "    GPG: Good signature from \"%s\"\n", uid)
	}
}

func runAdminCleanup(ctx *cli.Context) error {
	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}
	return s.Cleanup(true)
}

// adminIndexedDeployment implements ot_admin_get_indexed_deployment().
func adminIndexedDeployment(s *sysroot.Sysroot, index int) (*sysroot.Deployment, error) {
	return s.GetIndexedDeployment(index)
}

func runAdminSetDefault(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("INDEX must be specified")
	}
	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}
	index := atoi(ctx.Args[0])
	target, err := adminIndexedDeployment(s, index)
	if err != nil {
		return err
	}
	newDeployments := make([]*sysroot.Deployment, 0, len(s.Deployments))
	newDeployments = append(newDeployments, target)
	for i, d := range s.Deployments {
		if i == index {
			continue
		}
		newDeployments = append(newDeployments, d)
	}
	if err := s.WriteDeployments(newDeployments, sysroot.WriteDeploymentsOptions{DoPostclean: true}); err != nil {
		return err
	}
	if err := s.Cleanup(true); err != nil {
		return fmt.Errorf("Performing final cleanup: %w", err)
	}
	return nil
}

func runAdminUndeploy(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("INDEX must be specified")
	}
	indexStr := ctx.Args[0]
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 {
		return fmt.Errorf("Invalid index: %s", indexStr)
	}
	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}
	target, err := adminIndexedDeployment(s, index)
	if err != nil {
		return err
	}
	if target.Equal(s.Booted) {
		return fmt.Errorf("Cannot undeploy currently booted deployment %d", index)
	}
	newDeployments := make([]*sysroot.Deployment, 0, len(s.Deployments))
	for i, d := range s.Deployments {
		if i == index {
			continue
		}
		newDeployments = append(newDeployments, d)
	}
	if err := s.WriteDeployments(newDeployments, sysroot.WriteDeploymentsOptions{DoPostclean: true}); err != nil {
		return err
	}
	fmt.Fprintf(ctx.Stdout, "Deleted deployment %s.%d\n", target.Checksum, target.DeploySerial)
	if err := s.Cleanup(true); err != nil {
		return fmt.Errorf("Performing final cleanup: %w", err)
	}
	return nil
}

func adminPinOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:    "unpin",
			ShortName:   'u',
			ArgType:     cli.ArgNone,
			Description: "Unset pin",
		},
	}
}

// adminDeploymentIndexForType implements get_deployment_index_for_type().
func adminDeploymentIndexForType(s *sysroot.Sysroot, typ string) int64 {
	if s.Booted != nil && strings.HasPrefix(typ, "b") {
		return int64(s.Booted.Index)
	}
	var pending, rollback *sysroot.Deployment
	if s.Booted != nil {
		pending, rollback = s.QueryDeploymentsFor("")
	}
	if pending != nil && strings.HasPrefix(typ, "p") {
		return int64(pending.Index)
	}
	if rollback != nil && strings.HasPrefix(typ, "r") {
		return int64(rollback.Index)
	}
	return -1
}

func runAdminPin(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("INDEX must be specified")
	}
	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}
	unpin := ctx.Options.Has("unpin")
	for _, arg := range ctx.Args {
		var index int64
		switch arg {
		case "booted", "pending", "rollback":
			index = adminDeploymentIndexForType(s, arg)
			if index < 0 {
				return fmt.Errorf("Deployment type not found: %s", arg)
			}
		default:
			n, perr := strconv.ParseInt(arg, 10, 64)
			if perr != nil {
				if strings.Contains(perr.Error(), "value out of range") {
					return fmt.Errorf("Index too large: %s", arg)
				}
				return fmt.Errorf("Invalid index: %s", arg)
			}
			index = n
		}
		target, err := adminIndexedDeployment(s, int(index))
		if err != nil {
			return err
		}
		currentPin := target.IsPinned()
		desiredPin := !unpin
		if currentPin == desiredPin {
			state := "unpinned"
			if currentPin {
				state = "pinned"
			}
			fmt.Fprintf(ctx.Stdout, "Deployment %d is already %s\n", index, state)
			continue
		}
		if err := s.SetPinned(target, desiredPin); err != nil {
			return err
		}
		state := "unpinned"
		if desiredPin {
			state = "pinned"
		}
		fmt.Fprintf(ctx.Stdout, "Deployment %d is now %s\n", index, state)
	}
	return nil
}

func adminKargsOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:       "append-if-missing",
			ArgType:        cli.ArgStringArray,
			ArgDescription: "NAME=VALUE",
			Description:    "Append kernel arguments if they do not exist",
		},
	}
}

func runAdminKargs(ctx *cli.Context) error {
	args := ctx.Args
	if len(args) == 0 {
		writeKargsHelp(ctx.Stderr)
		return fmt.Errorf(`No "admin kargs" subcommand specified`)
	}
	sub := args[0]
	if sub != "edit-in-place" {
		writeKargsHelp(ctx.Stderr)
		return fmt.Errorf(`Unknown "admin kargs" subcommand '%s'`, sub)
	}
	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}
	if len(s.Deployments) == 0 {
		return fmt.Errorf("Unable to find a deployment in sysroot")
	}
	appends := ctx.Options.Strings("append-if-missing")
	for _, d := range s.Deployments {
		if d.BootConfig == nil {
			d.BootConfig = bootconfig.New()
		}
		kargs := bootconfig.FromString(d.BootConfig.Get("options"))
		for _, arg := range appends {
			kargs.AppendIfMissing(arg)
		}
		if err := s.SetDeploymentKargs(d, kargs.String()); err != nil {
			return err
		}
	}
	return nil
}

func writeKargsHelp(w io.Writer) {
	fmt.Fprintf(w, "Usage:\n  ostree admin kargs COMMAND\n\n")
	fmt.Fprintf(w, "Builtin \"admin kargs\" Commands:\n")
	fmt.Fprintf(w, "  edit-in-place             Set new kernel command line arguments in place (applies to all deployments by default)\n")
	fmt.Fprintf(w, "\nHelp Options:\n  -h, --help                Show help options\n")
	fmt.Fprintf(w, "\nApplication Options:\n")
	fmt.Fprintf(w, "  --sysroot=PATH            Create a new OSTree sysroot at PATH\n")
}

func adminInstutilOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:    "import-proc-cmdline",
			ArgType:     cli.ArgNone,
			Description: "Import current /proc/cmdline",
		},
		{
			LongName:    "merge",
			ArgType:     cli.ArgNone,
			Description: "Merge with previous command line",
		},
		{
			LongName:       "replace",
			ArgType:        cli.ArgStringArray,
			ArgDescription: "NAME=VALUE",
			Description:    "Set kernel argument, like root=/dev/sda1; this overrides any earlier argument with the same name",
		},
		{
			LongName:       "append",
			ArgType:        cli.ArgStringArray,
			ArgDescription: "NAME=VALUE",
			Description:    "Append kernel argument; useful with e.g. console= that can be used multiple times",
		},
	}
}

func writeInstutilHelp(w io.Writer) {
	fmt.Fprintf(w, "Usage:\n  ostree admin instutil COMMAND\n\n")
	fmt.Fprintf(w, "Builtin \"admin instutil\" Commands:\n")
	fmt.Fprintf(w, "  set-kargs                 Set kernel arguments\n")
	fmt.Fprintf(w, "\nHelp Options:\n  -h, --help                Show help options\n")
	fmt.Fprintf(w, "\nApplication Options:\n")
	fmt.Fprintf(w, "  --sysroot=PATH            Create a new OSTree sysroot at PATH\n")
}

func runAdminInstutil(ctx *cli.Context) error {
	args := ctx.Args
	if len(args) == 0 {
		writeInstutilHelp(ctx.Stderr)
		return fmt.Errorf(`No "admin instutil" subcommand specified`)
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "set-kargs":
		return runAdminInstutilSetKargs(ctx, rest)
	}
	writeInstutilHelp(ctx.Stderr)
	return fmt.Errorf(`Unknown "admin instutil" subcommand '%s'`, sub)
}

func runAdminInstutilSetKargs(ctx *cli.Context, args []string) error {
	s, err := openSysroot(ctx)
	if err != nil {
		return err
	}
	if len(s.Deployments) == 0 {
		return fmt.Errorf("Unable to find a deployment in sysroot")
	}
	first := s.Deployments[0]
	kargs := bootconfig.NewKernelArgs()
	switch {
	case ctx.Options.Has("import-proc-cmdline"):
		kargs, err = procCmdlineKargs()
		if err != nil {
			return err
		}
	case ctx.Options.Has("merge"):
		if first.BootConfig != nil {
			kargs = bootconfig.FromString(first.BootConfig.Get("options"))
		}
	}
	if replace := ctx.Options.Strings("replace"); len(replace) > 0 {
		kargs.ReplaceArgv(replace)
	}
	if append := ctx.Options.Strings("append"); len(append) > 0 {
		kargs.AppendArgv(append)
	}
	for _, arg := range args {
		kargs.Append(arg)
	}
	return s.RewriteDeploymentKargs(first, kargs.Strv())
}

func adminUnlockOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:    "hotfix",
			ArgType:     cli.ArgNone,
			Description: "Retain changes across reboots",
		},
		{
			LongName:    "transient",
			ArgType:     cli.ArgNone,
			Description: "Mount overlayfs read-only by default",
		},
	}
}

func runAdminUnlock(ctx *cli.Context) error {
	if len(ctx.Args) > 0 {
		return fmt.Errorf("This command takes no extra arguments")
	}
	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}
	booted, err := s.RequireBootedDeployment()
	if err != nil {
		return err
	}
	hotfix := ctx.Options.Has("hotfix")
	transient := ctx.Options.Has("transient")
	if hotfix && transient {
		return fmt.Errorf("Cannot specify both --hotfix and --transient")
	}
	state := "development"
	switch {
	case hotfix:
		state = "hotfix"
	case transient:
		state = "transient"
	}
	if err := s.Unlock(booted, state); err != nil {
		return err
	}
	switch state {
	case "hotfix":
		fmt.Fprint(ctx.Stdout, "Hotfix mode enabled.  A writable overlayfs is now mounted on /usr\n"+
			"for this booted deployment.  A non-hotfixed clone has been created\n"+
			"as the non-default rollback target.\n")
	case "development":
		fmt.Fprint(ctx.Stdout, "Development mode enabled.  A writable overlayfs is now mounted on /usr.\n"+
			"All changes there will be discarded on reboot.\n")
	case "transient":
		fmt.Fprint(ctx.Stdout, "A writable overlayfs is prepared for /usr, but is mounted read-only by default.\n"+
			"All changes there will be discarded on reboot.\n")
	}
	return nil
}

func adminLockFinalizationOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:    "unlock",
			ShortName:   'u',
			ArgType:     cli.ArgNone,
			Description: "Unlock finalization",
		},
	}
}

func runAdminLockFinalization(ctx *cli.Context) error {
	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}
	staged := s.GetStagedDeployment()
	if staged == nil {
		return fmt.Errorf("No staged deployment")
	}
	if err := s.ChangeFinalization(staged); err != nil {
		return err
	}
	if ctx.Options.Has("unlock") {
		fmt.Fprintln(ctx.Stdout, "Staged deployment is now queued to apply on shutdown")
	} else {
		fmt.Fprintln(ctx.Stdout, "Staged deployment is now finalization locked")
	}
	return nil
}

func runAdminFinalizeStaged(ctx *cli.Context) error {
	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}
	if s.GetStagedDeployment() == nil {
		return fmt.Errorf("No staged deployment")
	}
	return fmt.Errorf("Finalizing staged deployments is not supported yet")
}

func runAdminPostCopy(ctx *cli.Context) error {
	s, err := openSysrootLocked(ctx)
	if err != nil {
		return err
	}
	// Re-run the deployment refs and cleanup so that a copied sysroot is
	// consistent again.
	return s.Cleanup(false)
}

func runAdminStateOverlay(ctx *cli.Context) error {
	return fmt.Errorf("state-overlay is not supported yet")
}

var _ = repo.ModeBare
