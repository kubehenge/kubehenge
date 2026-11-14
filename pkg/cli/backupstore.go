package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kubehenge/kubehenge/pkg/backupstore"
)

func newBackupStoreCommand() *cobra.Command {
	var listen, dir string
	cmd := &cobra.Command{
		Use:   "backup-store",
		Short: "Serve the management cluster's backup volume to the appliance and the controller",
		Long: `Serve the management cluster's backup volume, mounted at --dir, over HTTP. It runs in the
management cluster. Every call carries the token in KUBEHENGE_BACKUP_STORE_TOKEN, sent in the
` + backupstore.TokenHeader + ` header.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBackupStore(cmd.Context(), listen, dir, os.Getenv("KUBEHENGE_BACKUP_STORE_TOKEN"))
		},
	}
	f := cmd.Flags()
	f.StringVar(&listen, "listen", ":8080", "address to serve on")
	f.StringVar(&dir, "dir", "/backups", "the backup volume's mount point")
	return cmd
}

func runBackupStore(ctx context.Context, listen, dir, token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("KUBEHENGE_BACKUP_STORE_TOKEN is empty: set it to the store's token")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("--dir %s is not a directory: mount the backup volume there", dir)
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           (&backupstore.Server{Dir: dir, Token: strings.TrimSpace(token)}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown) //nolint:contextcheck // the shutdown outlives the cancelled context
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving the backup volume: %w", err)
	}
	return nil
}
