package cmd

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/komari-monitor/komari/internal/diagnosticrecords"
	"github.com/spf13/cobra"
)

func init() {
	var receiptKey string
	command := &cobra.Command{
		Use:   "verify-forensic-archive FILE.tar.gz",
		Short: "Verify a forensic archive using an independently trusted receipt public key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := base64.StdEncoding.DecodeString(receiptKey)
			if err != nil || len(key) != ed25519.PublicKeySize {
				return errors.New("supply a base64 Ed25519 receipt public key")
			}
			file, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer file.Close()
			header, receipt, _, err := diagnosticrecords.VerifyBundle(file, ed25519.PublicKey(key))
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Verified node=%x stream=%x sequence=%d received_at=%s ciphertext_sha256=%x\n",
				header.Node, header.Stream, header.Sequence, receipt.ReceivedAt.UTC().Format(time.RFC3339Nano), receipt.Digest)
			return err
		},
	}
	command.Flags().StringVar(&receiptKey, "receipt-public-key", "", "independently trusted Ed25519 receipt public key in base64")
	RootCmd.AddCommand(command)
}
