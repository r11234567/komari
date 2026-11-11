//go:build cgo

package historyexport

import (
	"errors"
	"github.com/mattn/go-sqlite3"
)

func transientDatabaseBusy(err error) bool {
	var target sqlite3.Error
	return errors.As(err, &target) && (target.Code == sqlite3.ErrBusy || target.Code == sqlite3.ErrLocked)
}
