package models

import (
	"testing"
	"unsafe"
)

func TestPingRecordStaysLightweight(t *testing.T) {
	if size := unsafe.Sizeof(PingRecord{}); size > 128 {
		t.Fatalf("PingRecord size = %d bytes; history scans must not embed full Client and PingTask values", size)
	}
}
