package connectapi

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func TestMetricAdmissionFailsFastAndKeepsRecentLaneAvailable(t *testing.T) {
	var held []func()
	defer func() {
		for _, release := range held {
			release()
		}
	}()
	for range connectMetricHeavyCapacity {
		release, err := acquireMetricReadSlot(context.Background(), 2)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, release)
		// Each historical request may consume the whole weighted capacity.
		if connectMetricHeavyCapacity >= 2 {
			break
		}
	}
	started := time.Now()
	_, err := acquireMetricReadSlot(context.Background(), 2)
	var failure *connect.Error
	if !errors.As(err, &failure) || failure.Code() != connect.CodeResourceExhausted || failure.Meta().Get("Retry-After") == "" {
		t.Fatalf("missing bounded admission response: %v", err)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("overload queued physical reads")
	}
	release, err := acquireMetricReadSlot(context.Background(), 1)
	if err != nil {
		t.Fatalf("historical reads starved the recent lane: %v", err)
	}
	release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acquireMetricReadSlot(ctx, 1); connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("client cancellation became overload: %v", err)
	}
}

func TestMetricCooldownReturnsRetryAfter(t *testing.T) {
	connectMetricReadCooldown.Lock()
	previous := connectMetricReadCooldown.heavyUntil
	connectMetricReadCooldown.heavyUntil = time.Now().Add(4 * time.Second)
	connectMetricReadCooldown.Unlock()
	defer func() {
		connectMetricReadCooldown.Lock()
		connectMetricReadCooldown.heavyUntil = previous
		connectMetricReadCooldown.Unlock()
	}()
	_, err := acquireMetricReadSlot(context.Background(), 2)
	var failure *connect.Error
	if !errors.As(err, &failure) || failure.Meta().Get("Retry-After") != "4" {
		t.Fatalf("slow read cooldown was bypassed: %v", err)
	}
}

func TestPingSummarySharesReadCacheAndCancelsPhysicalWork(t *testing.T) {
	resetMetricQueryCache(t)
	var calls atomic.Int32
	load := func() (proto.Message, error) {
		calls.Add(1)
		return &metricsv1.GetPingStatsResponse{}, nil
	}
	for range 2 {
		message, err := cachedMetricRead(context.Background(), "ping:summary-test", load)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := message.(*metricsv1.GetPingStatsResponse); !ok {
			t.Fatal("wrong cached response type")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("summary repeated its physical read")
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	finished := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := cachedMetricRead(ctx, "ping:cancel-test", func() (proto.Message, error) {
			close(started)
			<-ctx.Done()
			close(finished)
			return nil, ctx.Err()
		})
		done <- err
	}()
	<-started
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("abandoned shared read kept running")
	}
	if err := <-done; connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("wrong cancellation: %v", err)
	}
}
