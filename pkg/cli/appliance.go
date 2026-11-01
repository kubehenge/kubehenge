package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
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
	// applianceRequestTimeout bounds one request to the interface. Setting the backups up writes
	// to the target before it answers.
	applianceRequestTimeout = time.Minute
	// maxApplianceAnswer bounds what is read of one answer.
	maxApplianceAnswer = 4 << 20
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
	check        appliance.ServiceCheck
	user         string
	passwordFile string
	totp         string
	json         bool
}

// applianceSession is a signed-in session with the interface.
type applianceSession struct {
	http *http.Client
	base string
	csrf string
}

// errSkipPrint ends a command whose answer was printed as JSON.
var errSkipPrint = errors.New("printed")

func newApplianceCommand() *cobra.Command {
	o := &applianceOptions{check: appliance.ServiceCheck{Wait: healthWait, Settle: healthSettle}}
	cmd := &cobra.Command{
		Use:   "appliance",
		Short: "Look after the appliance from its shell, as its Administration pages do",
		Long: `Look after the appliance from its shell, as its Administration pages do.

Run it in the appliance. Every command except health signs in to the interface as --user, with the
password from --password-file or KUBEHENGE_PASSWORD. The user's privileges and the interface's
checks apply as on its pages.`,
	}
	f := cmd.PersistentFlags()
	f.StringVar(&o.check.CertFile, "cert", "/etc/kubehenge/tls.crt", "certificate the interface must present")
	f.StringVar(&o.check.Address, "address", "127.0.0.1:443", "where the interface listens")
	cmd.AddCommand(o.healthCommand(), o.upgradesCommand(), o.upgradeCommand(), o.etcdBackupsCommand(), o.notificationsCommand())
	return cmd
}

// signInFlags adds the flags of a command that signs in to the interface.
func (o *applianceOptions) signInFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&o.user, "user", os.Getenv("KUBEHENGE_USER"), "Proxmox VE user to sign in as, such as root@pam (env KUBEHENGE_USER)")
	f.StringVar(&o.passwordFile, "password-file", "", "file holding the user's password, else KUBEHENGE_PASSWORD holds it")
	f.StringVar(&o.totp, "totp", "", "a TOTP code, for an account with a second factor")
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
			return o.check.Healthy(cmd.Context())
		},
	}
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
	o.signInFlags(cmd)
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
	o.signInFlags(cmd)
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
	o.signInFlags(cmd)
	o.jsonFlag(cmd)
	cmd.AddCommand(o.setUpBackupsCommand(), o.backupsOffCommand(), o.snapshotsCommand(), o.takeSnapshotCommand(), o.restoreEtcdCommand())
	return cmd
}

func printBackups(cmd *cobra.Command, v *appliance.EtcdBackupStatus) error {
	t := table(cmd)
	if v.Settings == nil {
		row(t, "Backups", "Off")
		return flush(t)
	}
	row(t, "Target", v.Settings.Target)
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
	}
	var secretFile, passphraseFile string
	cmd := &cobra.Command{
		Use:   "set-up",
		Short: "Back the management cluster's etcd up to a backup target on a schedule",
		Long: `Back the management cluster's etcd up to a backup target on a schedule. The state passphrase
comes from --passphrase-file or KUBEHENGE_PASSPHRASE. The target's secret access key comes from
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
	_ = cmd.MarkFlagRequired("target")
	o.signInFlags(cmd)
	o.jsonFlag(cmd)
	return cmd
}

const passphraseFileUsage = "file holding the state passphrase, else KUBEHENGE_PASSPHRASE holds it"

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
	o.signInFlags(cmd)
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
	o.signInFlags(cmd)
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
	o.signInFlags(cmd)
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
	wait := true
	var passphraseFile string
	cmd := &cobra.Command{
		Use:   "restore <snapshot>",
		Short: "Restore the management cluster's etcd from a snapshot, losing every change since",
		Long: `Restore the management cluster's etcd from a snapshot. It wipes etcd on every control-plane
node and restores the snapshot. Every cluster and every change made since goes back to how it was
then. The state passphrase comes from --passphrase-file or KUBEHENGE_PASSPHRASE.`,
		Args: argsSay(cobra.ExactArgs(1), "give the snapshot's name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			passphrase, err := secretFrom(passphraseFile, "KUBEHENGE_PASSPHRASE")
			if err != nil {
				return err
			}
			return o.signedIn(cmd, func(ctx context.Context, s *applianceSession) error {
				path := "/api/appliance/etcd-backups/snapshots/" + url.PathEscape(args[0]) + "/restore"
				if _, err := s.call(ctx, http.MethodPost, path, map[string]string{"passphrase": passphrase}, nil); err != nil {
					return err
				}
				if !wait {
					return say(cmd, "Restoring from "+args[0])
				}
				if err := s.untilJobEnds(ctx, cmd.OutOrStdout()); err != nil {
					return err
				}
				return say(cmd, "Restored from "+args[0])
			})
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", true, "wait until the restore ends, printing its log")
	cmd.Flags().StringVar(&passphraseFile, "passphrase-file", "", passphraseFileUsage)
	o.signInFlags(cmd)
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

// signedIn signs in, does what the command asks and signs out again.
func (o *applianceOptions) signedIn(cmd *cobra.Command, do func(context.Context, *applianceSession) error) error {
	ctx := cmd.Context()
	s, err := o.signIn(ctx)
	if err != nil {
		return err
	}
	defer func() { _, _ = s.call(context.WithoutCancel(ctx), http.MethodDelete, "/api/session", nil, nil) }()
	err = do(ctx, s)
	if errors.Is(err, errSkipPrint) {
		return nil
	}
	return err
}

func (o *applianceOptions) signIn(ctx context.Context) (*applianceSession, error) {
	if o.user == "" {
		return nil, errors.New("--user is required (env KUBEHENGE_USER)")
	}
	password, err := secretFrom(o.passwordFile, "KUBEHENGE_PASSWORD")
	if err != nil {
		return nil, err
	}
	if password == "" {
		return nil, errors.New("no password: give --password-file or KUBEHENGE_PASSWORD")
	}
	client, base, err := o.check.Client()
	if err != nil {
		return nil, fmt.Errorf("reaching the interface: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("a cookie jar: %w", err)
	}
	client.Jar, client.Timeout = jar, applianceRequestTimeout
	s := &applianceSession{http: client, base: base}
	var sess struct {
		CSRF         string `json:"csrf"`
		SecondFactor any    `json:"secondFactor"`
	}
	if _, err := s.call(ctx, http.MethodPost, "/api/session", map[string]string{"username": o.user, "password": password}, &sess); err != nil {
		return nil, err
	}
	s.csrf = sess.CSRF
	if sess.SecondFactor == nil {
		return s, nil
	}
	if o.totp == "" {
		return nil, fmt.Errorf("%s has a second factor: give --totp", o.user)
	}
	if _, err := s.call(ctx, http.MethodPost, "/api/session/second-factor", map[string]string{"kind": "totp", "code": o.totp}, &sess); err != nil {
		return nil, err
	}
	s.csrf = sess.CSRF
	return s, nil
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

// call sends a request with body as JSON and decodes the answer into out, when given. A refusal
// comes back as the interface's own message.
func (s *applianceSession) call(ctx context.Context, method, path string, body, out any) ([]byte, error) {
	var payload io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding the request: %w", err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, payload)
	if err != nil {
		return nil, fmt.Errorf("a request to the interface: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.csrf != "" {
		req.Header.Set("X-Kubehenge-Csrf", s.csrf)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching the interface: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxApplianceAnswer))
	if err != nil {
		return nil, fmt.Errorf("reading the interface's answer: %w", err)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		var refusal struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &refusal) == nil && refusal.Error != "" {
			return nil, errors.New(refusal.Error)
		}
		return nil, fmt.Errorf("the interface answered %s", resp.Status)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("the interface's answer: %w", err)
		}
	}
	return raw, nil
}

// secretFrom reads a secret from file, when named, else from the environment variable env.
func secretFrom(file, env string) (string, error) {
	if file == "" {
		return os.Getenv(env), nil
	}
	raw, err := os.ReadFile(file) //nolint:gosec // the file the person running the command named
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", file, err)
	}
	return string(trimNewline(raw)), nil
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
