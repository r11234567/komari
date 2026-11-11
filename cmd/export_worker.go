package cmd

import (
	"context"
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
		q, e := historyexport.Open(dir)
		if e != nil {
			return e
		}
		defer q.Close()
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		w := historyexport.Worker{Queue: q, InstanceCgroup: instance, IOBytesPerSecond: ioRate}
		e = w.Run(ctx)
		if e == context.Canceled {
			return nil
		}
		return e
	}}
	cmd.Flags().StringVar(&dir, "queue", "./data/exports-v2", "Private shared export queue directory")
	cmd.Flags().StringVar(&instance, "instance-cgroup", "", "Absolute cgroup-v2 directory of the main Komari instance (not the export worker)")
	cmd.Flags().Int64Var(&ioRate, "io-bytes-per-second", 8<<20, "Shared cooperative logical I/O budget, 64 KiB/s to 64 MiB/s; includes SQLite and cache reads")
	RootCmd.AddCommand(cmd)
}
