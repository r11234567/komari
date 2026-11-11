//go:build !cgo

package historyexport

// The SQLite driver is unavailable without CGO. Such deployments cannot run
// the strict export worker, but the main application must remain buildable.
func transientDatabaseBusy(error) bool { return false }
