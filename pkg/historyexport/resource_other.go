//go:build !linux

package historyexport

import "errors"

func SelfCgroup() (string, error) {
	return "", errors.New("strict background export currently requires Linux cgroup v2")
}
func VerifyResources(worker, instance string) (ResourceProof, error) {
	return ResourceProof{}, errors.New("strict background export requires a verified process-group hard CPU cap; this platform is not yet supported")
}
func lockWorker(dir string) (func(), error) {
	return nil, errors.New("strict background export worker is unsupported on this platform")
}
func diskAvailable(path string) (uint64, error) {
	return 0, errors.New("strict background export is unsupported on this platform")
}
func processIOBytes() (uint64, error) {
	return 0, errors.New("strict background I/O accounting is unsupported on this platform")
}

func readResourceCounters(group string) (resourceCounters, error) {
	return resourceCounters{}, errors.New("strict background resource accounting requires Linux cgroup v2")
}
