package cmd

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/komari-monitor/komari/internal/diagnosticrecords"
	"github.com/r11234567/komari-proto/diagnostics"
	"github.com/spf13/cobra"
)

func init() {
	var receiptKey, decryptionKeyPath, recordPath string
	command := &cobra.Command{
		Use:   "verify-forensic-archive FILE.tar.gz",
		Short: "Verify a forensic archive using an independently trusted receipt public key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (decryptionKeyPath == "") != (recordPath == "") {
				return errors.New("supply both --decryption-key-file and --record-output to open a verified archive")
			}
			key, err := base64.StdEncoding.DecodeString(receiptKey)
			if err != nil || len(key) != ed25519.PublicKeySize {
				return errors.New("supply a base64 Ed25519 receipt public key")
			}
			file, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer file.Close()
			header, receipt, envelope, err := diagnosticrecords.VerifyBundle(file, ed25519.PublicKey(key))
			if err != nil {
				return err
			}
			if recordPath != "" {
				if err := openVerifiedForensicRecord(decryptionKeyPath, recordPath, envelope); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Verified node=%x stream=%x sequence=%d received_at=%s ciphertext_sha256=%x\n",
				header.Node, header.Stream, header.Sequence, receipt.ReceivedAt.UTC().Format(time.RFC3339Nano), receipt.Digest)
			return err
		},
	}
	command.Flags().StringVar(&receiptKey, "receipt-public-key", "", "independently trusted Ed25519 receipt public key in base64")
	command.Flags().StringVar(&decryptionKeyPath, "decryption-key-file", "", "local panel X25519 private key file")
	command.Flags().StringVar(&recordPath, "record-output", "", "new private file for the decrypted record")
	RootCmd.AddCommand(command)
	RootCmd.AddCommand(&cobra.Command{
		Use:   "init-forensic-keys PRIVATE_DIRECTORY",
		Short: "Create panel forensic keys and print their public halves",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return initForensicKeys(args[0], cmd.OutOrStdout())
		},
	})
}

func initForensicKeys(path string, out io.Writer) error {
	dir, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	info, err := dir.Stat(".")
	if err != nil || info.Mode().Perm() != 0o700 {
		return errors.New("key directory must have mode 0700")
	}
	recipientPublic, recipientPrivate, err := diagnostics.GenerateKey()
	if err != nil {
		return err
	}
	defer clear(recipientPrivate)
	receiptPublic, receiptPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	defer clear(receiptPrivate)
	const recipientName, receiptName = "forensic-recipient.key", "forensic-receipt.key"
	if err := writeNewForensicKey(dir, recipientName, recipientPrivate); err != nil {
		return err
	}
	if err := writeNewForensicKey(dir, receiptName, receiptPrivate); err != nil {
		return errors.Join(err, dir.Remove(recipientName))
	}
	_, err = fmt.Fprintf(out, "Recipient public key: %s\nReceipt public key: %s\n", base64.StdEncoding.EncodeToString(recipientPublic), base64.StdEncoding.EncodeToString(receiptPublic))
	return err
}

func writeNewForensicKey(dir *os.Root, name string, key []byte) error {
	file, err := dir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(key)
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		return errors.Join(err, dir.Remove(name))
	}
	return nil
}

func openVerifiedForensicRecord(keyPath, recordPath string, envelope []byte) error {
	file, err := os.Open(keyPath)
	if err != nil {
		return err
	}
	defer file.Close()
	const keySize = 32
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || (info.Mode().Perm() != 0o400 && info.Mode().Perm() != 0o600) {
		return errors.New("decryption key must be a private regular file")
	}
	key, err := io.ReadAll(io.LimitReader(file, keySize+1))
	defer clear(key)
	if err != nil || len(key) != keySize {
		return diagnostics.ErrKey
	}
	_, record, err := diagnostics.Open(key, envelope)
	if err != nil {
		return err
	}
	defer clear(record)
	output, err := os.OpenFile(recordPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = output.Write(record)
	if err = errors.Join(err, output.Sync(), output.Close()); err != nil {
		return errors.Join(err, os.Remove(recordPath))
	}
	return nil
}
