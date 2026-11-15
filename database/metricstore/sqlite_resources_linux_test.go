//go:build linux

package metricstore

import (
	"reflect"
	"testing"
)

func TestCgroupV2MembershipHonorsServiceAndParents(t *testing.T) {
	for _, tc := range []struct {
		membership string
		want       []string
	}{
		{"0::/\n", []string{"/sys/fs/cgroup"}},
		{"0::/system.slice/komari.service\n", []string{"/sys/fs/cgroup", "/sys/fs/cgroup/system.slice/komari.service", "/sys/fs/cgroup/system.slice"}},
		{"5:memory:/old\n0::/docker/panel\n", []string{"/sys/fs/cgroup", "/sys/fs/cgroup/docker/panel", "/sys/fs/cgroup/docker"}},
		{"0::/../../escape\n", []string{"/sys/fs/cgroup"}},
		{"malformed\n", []string{"/sys/fs/cgroup"}},
	} {
		if got := cgroupV2Directories(tc.membership); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("membership %q: got %v; want %v", tc.membership, got, tc.want)
		}
	}
}
