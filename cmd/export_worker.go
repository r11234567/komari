package cmd

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/komari-monitor/komari/pkg/historyexport"
	"github.com/spf13/cobra"
)

func init() {
	var dir, instance string
	var ioRate int64
	cmd := &cobra.Command{Use: "export-worker", Short: "Run durable background CSV tasks in a verified, isolated Linux CPU group", RunE: func(cmd *cobra.Command, args []string) error {
		// Verify before opening SQLite, decoding job metadata or planning input.
		self, e := historyexport.SelfCgroup()
		if e != nil {
			return e
		}
		if _, e = historyexport.VerifyResources(self, instance); e != nil {
			return e
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		for {
			q, err := historyexport.OpenWorkerQueue(ctx, dir)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if err != nil {
				return err
			}
			w := historyexport.Worker{Queue: q, InstanceCgroup: instance, IOBytesPerSecond: ioRate}
			err = w.Run(ctx)
			closeErr := q.Close()
			if errors.Is(err, context.Canceled) {
				return closeErr
			}
			if !errors.Is(err, historyexport.ErrQueueReplaced) || closeErr != nil {
				return errors.Join(err, closeErr)
			}
		}
	}}
	cmd.Flags().StringVar(&dir, "queue", "./data/exports-v2", "Private shared export queue directory")
	cmd.Flags().StringVar(&instance, "instance-cgroup", "", "Absolute cgroup-v2 directory of the main Komari instance (not the export worker)")
	cmd.Flags().Int64Var(&ioRate, "io-bytes-per-second", 8<<20, "Shared cooperative logical I/O budget, 64 KiB/s to 64 MiB/s; includes SQLite and cache reads")
	RootCmd.AddCommand(cmd)
}
