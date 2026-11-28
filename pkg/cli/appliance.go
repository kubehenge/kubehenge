package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/appliance"
	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

const (
	// mib is the bytes of a MiB, the unit sizes are shown in.
	mib = 1 << 20
	// appliancePoll is how often a command that waits asks again.
	appliancePoll = 2 * time.Second
	// snapshotWait and restoreWait bound how long take and restore wait for the result.
	snapshotWait = 15 * time.Minute
	restoreWait  = time.Hour
	// tabWidth and columnGap lay the tables out as kubectl's are.
	tabWidth  = 4
	columnGap = 2
)

// applianceOptions are the flags of the appliance commands, which sign in to the appliance's
// interface and ask it what its pages ask, so that its checks and refusals hold for both.
type applianceOptions struct {
	check appliance.ServiceCheck
	json  bool
	// quiet is health's --quiet.
	quiet bool
}

// applianceSession is the client the commands reach the interface with, as the login's user.
type applianceSession = interfaceClient

// errSkipPrint ends a command whose answer was printed as JSON.
var errSkipPrint = errors.New("printed")

func newApplianceCommand() *cobra.Command {
	o := &applianceOptions{check: appliance.ServiceCheck{Wait: healthWait, Settle: healthSettle}}
	cmd := &cobra.Command{
		Use:   "appliance",
		Short: "Look after the appliance from its shell, as its Administration pages do",
		Long: `Look after the appliance from its shell, as its Administration pages do.

Run it in the appliance. Every command except health reaches the interface as the user
"kubehenge login" signed in. The user's privileges and the interface's checks apply as on its pages.

Sign in first, in the appliance with
"kubehenge login https://127.0.0.1 --user root@pam --password-file <file>".`,
	}
	cmd.AddCommand(o.healthCommand(), o.upgradesCommand(), o.upgradeCommand(), o.etcdBackupsCommand(), o.notificationsCommand(),
		o.activityCommand(), o.takeOverCommand(), o.readyForRebootCommand(), o.exportCommand(), o.importCommand(),
		o.settingsCommand(), o.certificateCommand(), o.statePassphraseCommand())
	return cmd
}

// errNotReadyForReboot ends ready-for-reboot with a blocker, so that the exit status is not 0.
var errNotReadyForReboot = errors.New("not ready for a node reboot")

func (o *applianceOptions) readyForRebootCommand() *cobra.Command {
	var node string
	cmd := &cobra.Command{
		Use:   "ready-for-reboot",
		Short: "Exit 0 only when a Proxmox VE node may be rebooted",
		Long: `Exit 0 only when every etcd member of the management cluster and of every workload cluster is
healthy. With --node, the guests of that Proxmox VE node must also be able to go away without costing
any etcd its quorum. Otherwise the first blocker is printed and the exit status is 1.

Patch the nodes one at a time. Run it, reboot one node, wait for it to pass again, then do the next.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				path := "/api/appliance/reboot-ready"
				if node != "" {
					path += "?node=" + url.QueryEscape(node)
				}
				var v struct {
					Answer provisioner.RebootAnswer `json:"answer"`
				}
				if _, err := s.call(ctx, http.MethodGet, path, nil, &v); err != nil {
					return err
				}
				if !v.Answer.Ready {
					return fmt.Errorf("%w: %s", errNotReadyForReboot, v.Answer.Blocker)
				}
				if node != "" {
					return say(cmd, "Ready to reboot "+node)
				}
				return say(cmd, "Ready for a node reboot")
			})
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "Proxmox VE node to be rebooted")
	return cmd
}

// jsonFlag adds --json to a command that prints what the interface answers.
func (o *applianceOptions) jsonFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&o.json, "json", false, "print the interface's answer as JSON")
}

func (o *applianceOptions) healthCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check that the interface answers and stays up, as after an upgrade",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.runHealth(cmd)
		},
	}
	cmd.Flags().BoolVar(&o.quiet, "quiet", false, "print nothing, for scripts: the exit status is the answer")
	cmd.Flags().BoolVar(&o.json, "json", false, "print the verdict as one JSON object")
	cmd.Flags().StringVar(&o.check.CertFile, "cert", appliance.InstallerCertificatePath, "certificate the interface must present")
	cmd.Flags().StringVar(&o.check.StoredFile, "stored-cert", appliance.InterfaceCertificatePath, "certificate an administrator gave the interface, which it must present instead while the file reads cleanly")
	cmd.Flags().StringVar(&o.check.Address, "address", "127.0.0.1:443", "where the interface listens")
	cmd.Flags().DurationVar(&o.check.Wait, "wait", healthWait, "how long the interface has to answer")
	cmd.Flags().DurationVar(&o.check.Settle, "settle", healthSettle, "how long the interface must stay up after it answers")
	return cmd
}

func (o *applianceOptions) upgradesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrades",
		Short: "Show the installed release, the newer ones and how the last upgrade went",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				var v struct {
					Version   string                  `json:"version"`
					Available []string                `json:"available"`
					Error     string                  `json:"error"`
					Status    *appliance.UpdateStatus `json:"status"`
				}
				if err := o.get(ctx, cmd, s, "/api/appliance/upgrades", &v); err != nil {
					return err
				}
				t := table(cmd)
				row(t, "Installed", v.Version)
				row(t, "Available", orNone(strings.Join(v.Available, ", ")))
				if v.Error != "" {
					row(t, "Index", v.Error)
				}
				for _, line := range upgradeLines(v.Status) {
					row(t, line[0], line[1])
				}
				return flush(t)
			})
		},
	}
	o.jsonFlag(cmd)
	return cmd
}

// upgradeLines says how the updater's last upgrade went.
func upgradeLines(st *appliance.UpdateStatus) [][2]string {
	if st == nil {
		return nil
	}
	at := st.Time.UTC().Format(time.RFC3339)
	var last string
	switch {
	case st.RolledBack:
		last = fmt.Sprintf("to %s rolled back to %s, %s", st.To, st.From, at)
	case st.State == appliance.StateFailed:
		last = fmt.Sprintf("to %s failed, %s", orNone(st.To), at)
	case st.State == appliance.StateUpdating:
		last = "from " + st.From + " under way"
	default:
		last = fmt.Sprintf("%s from %s to %s, %s", st.State, st.From, orNone(st.To), at)
	}
	lines := [][2]string{{"Last upgrade", last}}
	if st.Error != "" {
		lines = append(lines, [2]string{"Reason", st.Error})
	}
	return lines
}

func (o *applianceOptions) upgradeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade <version>",
		Short: "Upgrade the appliance to a newer signed release",
		Long: `Upgrade the appliance to a newer signed release. The interface restarts on the new release.
If it does not come up, the updater puts the old release back. "kubehenge appliance upgrades" shows
how the upgrade went.`,
		Args: argsSay(cobra.ExactArgs(1), "give the version to upgrade to"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				if _, err := s.call(ctx, http.MethodPost, "/api/appliance/upgrades", map[string]string{"version": args[0]}, nil); err != nil {
					return err
				}
				return say(cmd, "Upgrading to "+args[0])
			})
		},
	}
	return cmd
}

func (o *applianceOptions) etcdBackupsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "etcd-backups",
		Short: "Show how the management cluster's etcd is backed up",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				var v appliance.EtcdBackupStatus
				if err := o.get(ctx, cmd, s, "/api/appliance/etcd-backups", &v); err != nil {
					return err
				}
				return printBackups(cmd, &v)
			})
		},
	}
	o.jsonFlag(cmd)
	cmd.AddCommand(o.setUpBackupsCommand(), o.backupsOffCommand(), o.snapshotsCommand(), o.takeSnapshotCommand(), o.restoreEtcdCommand(),
		o.recreateCommand(), o.recoverCommand(), o.dismissWarningCommand())
	return cmd
}

// printBackupPlaces writes where the backups are and what the backup volume holds.
func printBackupPlaces(t *tabwriter.Writer, v *appliance.EtcdBackupStatus) {
	if v.Where != nil {
		row(t, "Backups in", strings.Join(v.Where.Where, ", "))
		last := "none yet"
		if v.Where.Last != nil {
			last = v.Where.Last.UTC().Format(time.RFC3339)
		}
		row(t, "Last backup", last)
		if v.Where.Error != "" {
			row(t, "Failed", v.Where.Error)
		}
	}
	vol := v.Volume
	if vol == nil {
		return
	}
	if vol.Last != nil {
		row(t, "Volume snapshot", vol.Last.Name)
	}
	if vol.Export != nil {
		row(t, "Volume export", vol.Export.Name)
	}
	if vol.Copy != nil {
		row(t, "Appliance copy", fmt.Sprintf("%s, %d of %d MiB", cmp.Or(vol.Copy.Snapshot, "none"), vol.Copy.Bytes/mib, vol.Copy.Cap/mib))
	}
}

func printBackups(cmd *cobra.Command, v *appliance.EtcdBackupStatus) error {
	t := table(cmd)
	printBackupPlaces(t, v)
	if v.Settings == nil {
		if v.Volume != nil {
			return flush(t)
		}
		row(t, "Backups", "Off")
		if v.Warning.Shown() {
			row(t, "Warning", "The management cluster's etcd is not backed up. Every cluster's CAs, kubeconfigs and backup keys live only there.")
		} else if v.Warning != nil && v.Warning.Dismissal != nil {
			d := v.Warning.Dismissal
			row(t, "Warning", fmt.Sprintf("Dismissed by %s on %s until %s", d.By, d.At.UTC().Format(time.DateOnly), d.Until.UTC().Format(time.DateOnly)))
		}
		return flush(t)
	}
	if v.ExportDue {
		row(t, "Warning", "No export of the appliance's state since the backup settings or the recovery key changed. Stored Secrets open only with this appliance's key: kubehenge appliance export")
	}
	row(t, "Target", v.Settings.Target)
	if len(v.Settings.SpecRecipients) > 0 {
		row(t, "Extra recipients", strings.Join(v.Settings.SpecRecipients, ", "))
	}
	row(t, "Schedule", fmt.Sprintf("every %d h, newest %d kept", v.Settings.EveryHours, v.Settings.Keep))
	if v.Last != nil {
		row(t, "Last snapshot", v.Last.Name)
	}
	if v.Taking {
		row(t, "Status", "Taking a snapshot")
	}
	if v.Error != "" {
		row(t, "Failed", v.Error)
	}
	if v.Next != nil {
		row(t, "Next", v.Next.UTC().Format(time.RFC3339))
	}
	return flush(t)
}

func (o *applianceOptions) setUpBackupsCommand() *cobra.Command {
	var req struct {
		Target          string `json:"target"`
		EveryHours      int    `json:"everyHours"`
		Keep            int    `json:"keep"`
		AccessKeyID     string `json:"accessKeyID,omitempty"`
		SecretAccessKey string `json:"secretAccessKey,omitempty"`
		Passphrase      string `json:"passphrase"`
		// SpecRecipients replace the stored ones, so that a run without any removes them.
		SpecRecipients []string `json:"specRecipients,omitempty"`
	}
	var secretFile, passphraseFile string
	cmd := &cobra.Command{
		Use:   "set-up",
		Short: "Back the management cluster's etcd up to a backup target on a schedule",
		Long: `Back the management cluster's etcd up to a backup target on a schedule. A passphrase the
appliance requires comes from --passphrase-file or KUBEHENGE_PASSPHRASE. The target's secret access key comes from
--secret-access-key-file or KUBEHENGE_SECRET_ACCESS_KEY. Without one, the stored key is kept.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			if req.Passphrase, err = secretFrom(passphraseFile, "KUBEHENGE_PASSPHRASE"); err != nil {
				return err
			}
			if req.SecretAccessKey, err = secretFrom(secretFile, "KUBEHENGE_SECRET_ACCESS_KEY"); err != nil {
				return err
			}
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				raw, err := s.call(ctx, http.MethodPut, "/api/appliance/etcd-backups", req, nil)
				if err != nil {
					return err
				}
				return o.printBackupsAnswer(cmd, raw)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Target, "target", "", "the backup target, by its name in the management cluster's settings")
	f.IntVar(&req.EveryHours, "every-hours", appliance.DefaultEtcdBackupHours, "hours between snapshots")
	f.IntVar(&req.Keep, "keep", appliance.DefaultEtcdBackupKeep, "how many snapshots to keep")
	f.StringVar(&req.AccessKeyID, "access-key-id", "", "the target's access key ID, or empty to keep the stored one")
	f.StringVar(&secretFile, "secret-access-key-file", "", "file holding the secret access key, else KUBEHENGE_SECRET_ACCESS_KEY holds it")
	f.StringVar(&passphraseFile, "passphrase-file", "", passphraseFileUsage)
	f.StringArrayVar(&req.SpecRecipients, "spec-recipient", nil, "an age public key (age1...) that every cluster spec's Secrets are also sealed to, repeatable")
	_ = cmd.MarkFlagRequired("target")
	o.jsonFlag(cmd)
	return cmd
}

const passphraseFileUsage = "file holding the state passphrase, where the appliance requires one, else KUBEHENGE_PASSPHRASE holds it"

func (o *applianceOptions) printBackupsAnswer(cmd *cobra.Command, raw []byte) error {
	if o.json {
		return printJSON(cmd, raw)
	}
	var v appliance.EtcdBackupStatus
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("the interface's answer: %w", err)
	}
	return printBackups(cmd, &v)
}

func (o *applianceOptions) backupsOffCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "off",
		Short: "Stop backing the management cluster's etcd up, keeping the snapshots in the target",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				if _, err := s.call(ctx, http.MethodDelete, "/api/appliance/etcd-backups", nil, nil); err != nil {
					return err
				}
				return say(cmd, "Backups off")
			})
		},
	}
	return cmd
}

func (o *applianceOptions) snapshotsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshots",
		Short: "List the management cluster's snapshots in the backup target, newest first",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				var v struct {
					Snapshots []provisioner.EtcdSnapshot `json:"snapshots"`
					Error     string                     `json:"error"`
				}
				if err := o.get(ctx, cmd, s, "/api/appliance/etcd-backups/snapshots", &v); err != nil {
					return err
				}
				if v.Error != "" {
					return errors.New(v.Error)
				}
				t := table(cmd)
				row(t, "NAME", "TAKEN", "KUBERNETES", "SIZE")
				for i := range v.Snapshots {
					sn := &v.Snapshots[i]
					row(t, sn.Name, sn.Time.UTC().Format(time.RFC3339), sn.KubernetesVersion, strconv.FormatInt(sn.Size, 10))
				}
				return flush(t)
			})
		},
	}
	o.jsonFlag(cmd)
	return cmd
}

func (o *applianceOptions) takeSnapshotCommand() *cobra.Command {
	wait := true
	cmd := &cobra.Command{
		Use:   "take",
		Short: "Take a snapshot of the management cluster's etcd now",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				var before appliance.EtcdBackupStatus
				if _, err := s.call(ctx, http.MethodGet, "/api/appliance/etcd-backups", nil, &before); err != nil {
					return err
				}
				if _, err := s.call(ctx, http.MethodPost, "/api/appliance/etcd-backups/snapshots", nil, nil); err != nil {
					return err
				}
				if !wait {
					return say(cmd, "Taking a snapshot")
				}
				taken, err := s.untilTaken(ctx, &before)
				if err != nil {
					return err
				}
				return say(cmd, "Taken: "+taken)
			})
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", true, "wait until the snapshot is stored")
	return cmd
}

// untilTaken waits for the snapshot asked for after before, and returns its name.
func (s *applianceSession) untilTaken(ctx context.Context, before *appliance.EtcdBackupStatus) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, snapshotWait)
	defer cancel()
	for {
		var now appliance.EtcdBackupStatus
		if _, err := s.call(ctx, http.MethodGet, "/api/appliance/etcd-backups", nil, &now); err != nil {
			return "", err
		}
		switch {
		case now.Taking:
		case now.Last != nil && (before.Last == nil || now.Last.Name != before.Last.Name):
			return now.Last.Name, nil
		case now.Failed != nil && (before.Failed == nil || now.Failed.After(*before.Failed)):
			return "", errors.New(now.Error)
		}
		if err := pause(ctx); err != nil {
			return "", fmt.Errorf("waiting for the snapshot: %w", err)
		}
	}
}

func (o *applianceOptions) restoreEtcdCommand() *cobra.Command {
	return o.snapshotRunCommand(&snapshotRun{
		use:   "restore <snapshot>",
		short: "Restore the management cluster's etcd from a snapshot, losing every change since",
		long: `Restore the management cluster's etcd from a snapshot. It wipes etcd on every control-plane
node and restores the snapshot. Every cluster and every change made since goes back to how it was
then. A passphrase the appliance requires comes from --passphrase-file or KUBEHENGE_PASSPHRASE.`,
		action: "restore", started: "Restoring from ", done: "Restored from ",
	})
}

func (o *applianceOptions) takeOverCommand() *cobra.Command {
	var passphraseFile string
	var overwrite bool
	cmd := &cobra.Command{
		Use:   "take-over",
		Short: "Apply this appliance's state after a newer one changed the management cluster",
		Long: `Make this appliance's state the one the management cluster follows. An apply is refused when
an apply this state does not know changed the cluster since, as after a snapshot rollback, an older
backup or a second appliance. Once taken over, the next apply undoes those changes. It needs
--overwrite. A passphrase the appliance requires comes from --passphrase-file or KUBEHENGE_PASSPHRASE.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !overwrite {
				return errors.New("take-over undoes the newer changes at the next apply. Give --overwrite to go ahead")
			}
			passphrase, err := secretFrom(passphraseFile, "KUBEHENGE_PASSPHRASE")
			if err != nil {
				return err
			}
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				if _, err := s.call(ctx, http.MethodPost, "/api/setup/take-over", map[string]string{"passphrase": passphrase}, nil); err != nil {
					return err
				}
				return say(cmd, "Taken over. The next apply brings the management cluster to this state")
			})
		},
	}
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "undo the newer changes at the next apply")
	cmd.Flags().StringVar(&passphraseFile, "passphrase-file", "", passphraseFileUsage)
	return cmd
}

func (o *applianceOptions) statePassphraseCommand() *cobra.Command {
	var passphraseFile, newFile string
	var require, off bool
	cmd := &cobra.Command{
		Use:   "state-passphrase",
		Short: "Require a passphrase to open the management cluster's state, or stop requiring one",
		Long: `Require a passphrase to open the management cluster's state, or stop requiring one. The appliance
keeps the state's key by default, and the signed-in user's privileges are the only check. With
--require, every action that opens the state asks for the new passphrase, from --new-passphrase-file or
KUBEHENGE_NEW_PASSPHRASE. Automatic upgrades then store it. With --off, the appliance keeps the key
again. The passphrase required so far comes from --passphrase-file or KUBEHENGE_PASSPHRASE.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if require == off {
				return errors.New("give --require or --off")
			}
			passphrase, err := secretFrom(passphraseFile, "KUBEHENGE_PASSPHRASE")
			if err != nil {
				return err
			}
			next := ""
			if require {
				if next, err = secretFrom(newFile, "KUBEHENGE_NEW_PASSPHRASE"); err != nil {
					return err
				}
			}
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				body := map[string]any{"required": require, passphraseField: passphrase, "newPassphrase": next}
				if _, err := s.call(ctx, http.MethodPut, "/api/setup/state-passphrase", body, nil); err != nil {
					return err
				}
				if require {
					return say(cmd, "Required. Every action that opens the management cluster's state asks for the passphrase")
				}
				return say(cmd, "The appliance keeps the state's key again")
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&require, "require", false, "require a new passphrase")
	f.BoolVar(&off, "off", false, "stop requiring a passphrase")
	f.StringVar(&passphraseFile, "passphrase-file", "", passphraseFileUsage)
	f.StringVar(&newFile, "new-passphrase-file", "", "file holding the new passphrase, else KUBEHENGE_NEW_PASSPHRASE holds it")
	return cmd
}

// untilJobEnds follows the appliance's run until it ends. It prints its log as it grows.
func (s *applianceSession) untilJobEnds(ctx context.Context, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, restoreWait)
	defer cancel()
	printed := 0
	for {
		var v struct {
			Job *appliance.JobView `json:"job"`
		}
		if _, err := s.call(ctx, http.MethodGet, "/api/setup", nil, &v); err != nil {
			return err
		}
		if v.Job == nil {
			return errors.New("the interface shows no run")
		}
		printed = printLog(out, v.Job, printed)
		switch v.Job.State {
		case appliance.StateSucceeded:
			return nil
		case appliance.StateFailed:
			return errors.New(v.Job.Error)
		}
		if err := pause(ctx); err != nil {
			return fmt.Errorf("waiting for the run: %w", err)
		}
	}
}

// printLog prints the lines of the run's log after the first printed, and returns how many there
// are. The view holds only the last lines of a long log.
func printLog(out io.Writer, job *appliance.JobView, printed int) int {
	total := max(job.Lines, len(job.Log))
	first := len(job.Log) - (total - printed)
	for _, line := range job.Log[max(first, 0):] {
		_, _ = fmt.Fprintln(out, line)
	}
	return total
}

// signedIn does what the command asks as the login's user.
func (*applianceOptions) signedIn(cmd *cobra.Command, do func(context.Context, *applianceSession) error) error {
	s, err := openAppliance(cmd.Context())
	if err != nil {
		return err
	}
	if err = do(cmd.Context(), s); errors.Is(err, errSkipPrint) {
		return nil
	}
	return err
}

// get reads path into v, or prints the answer as it is with --json and returns errSkipPrint.
func (o *applianceOptions) get(ctx context.Context, cmd *cobra.Command, s *applianceSession, path string, v any) error {
	raw, err := s.call(ctx, http.MethodGet, path, nil, v)
	if err != nil {
		return err
	}
	if o.json {
		if err := printJSON(cmd, raw); err != nil {
			return err
		}
		return errSkipPrint
	}
	return nil
}

// secretFrom reads a secret from file, when named, else from the environment variable env.
func secretFrom(file, env string) (string, error) {
	if file == "" {
		return os.Getenv(env), nil
	}
	raw, err := readNamedFile(file)
	if err != nil {
		return "", err
	}
	return string(trimNewline(raw)), nil
}

// readNamedFile reads a file the person running the command named. Its error names the file once,
// where the system's own names it twice.
func readNamedFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the file the person running the command named
	var pathErr *fs.PathError
	switch {
	case err == nil:
		return raw, nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%s does not exist. Give the path of an existing file", path)
	case errors.As(err, &pathErr):
		return nil, fmt.Errorf("reading %s: %w", path, pathErr.Err)
	}
	return nil, fmt.Errorf("reading %s: %w", path, err)
}

func pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // wrapped by the caller
	case <-time.After(appliancePoll):
		return nil
	}
}

func table(cmd *cobra.Command) *tabwriter.Writer {
	return tabwriter.NewWriter(cmd.OutOrStdout(), 0, tabWidth, columnGap, ' ', 0)
}

func row(t *tabwriter.Writer, cells ...string) {
	_, _ = fmt.Fprintln(t, strings.Join(cells, "\t"))
}

func flush(t *tabwriter.Writer) error {
	if err := t.Flush(); err != nil {
		return fmt.Errorf("writing the answer: %w", err)
	}
	return nil
}

func say(cmd *cobra.Command, line string) error {
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
		return fmt.Errorf("writing the answer: %w", err)
	}
	return nil
}

func printJSON(cmd *cobra.Command, raw []byte) error {
	if _, err := cmd.OutOrStdout().Write(append(bytes.TrimSpace(raw), '\n')); err != nil {
		return fmt.Errorf("writing the answer: %w", err)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "None"
	}
	return s
}
