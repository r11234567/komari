package dbcore

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestUnzipToDirRejectsTraversalAndSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		add  func(*zip.Writer) error
	}{
		{
			name: "traversal",
			add: func(zw *zip.Writer) error {
				entry, err := zw.Create("../outside")
				if err != nil {
					return err
				}
				_, err = entry.Write([]byte("outside"))
				return err
			},
		},
		{
			name: "symlink",
			add: func(zw *zip.Writer) error {
				header := &zip.FileHeader{Name: "link"}
				header.SetMode(os.ModeSymlink | 0777)
				entry, err := zw.CreateHeader(header)
				if err != nil {
					return err
				}
				_, err = entry.Write([]byte("../outside"))
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			archivePath := filepath.Join(base, "test.zip")
			archive, err := os.Create(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			zw := zip.NewWriter(archive)
			if err := tc.add(zw); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}

			destination := filepath.Join(base, "destination")
			if err := unzipToDir(archivePath, destination); err == nil {
				t.Fatalf("unzipToDir accepted %s entry", tc.name)
			}
		})
	}
}
