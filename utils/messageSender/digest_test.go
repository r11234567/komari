package messageSender

import (
	"sync"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/utils/messageSender/factory"
)

type recordingSender struct {
	mu     sync.Mutex
	events []models.EventMessage
}

func (r *recordingSender) GetName() string                         { return "recording" }
func (r *recordingSender) GetConfiguration() factory.Configuration { return &struct{}{} }
func (r *recordingSender) Init() error                             { return nil }
func (r *recordingSender) Destroy() error                          { return nil }
func (r *recordingSender) SendTextMessage(string, string) error    { return nil }
func (r *recordingSender) SendEvent(event models.EventMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := event
	copied.Clients = append([]models.Client(nil), event.Clients...)
	r.events = append(r.events, copied)
	return nil
}

func TestDigestMergesMatchingAlerts(t *testing.T) {
	sender := &recordingSender{}
	queued, _ := newDigestQueue()
	restore := installDigestTest(t, sender, true, queued)
	defer restore()

	first := models.EventMessage{
		Kind: KindOffline, Event: "Offline", Emoji: "🔴",
		Clients: []models.Client{{UUID: "a", Name: "alpha"}},
		Time:    time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC),
	}
	second := models.EventMessage{
		Kind: KindOffline, Event: "Offline", Emoji: "🔴",
		Clients: []models.Client{{UUID: "b", Name: "beta"}, {UUID: "a", Name: "alpha"}},
		Time:    time.Date(2026, 10, 5, 1, 0, 3, 0, time.UTC),
	}
	done := make(chan error, 2)
	joined := make(chan struct{}, 2)
	prevJoined := afterDigestEnqueue
	afterDigestEnqueue = func() { joined <- struct{}{} }
	defer func() { afterDigestEnqueue = prevJoined }()
	go func() { done <- SendEvent(first) }()
	go func() { done <- SendEvent(second) }()
	waitForDigest(t, joined, 2)
	if len(sender.events) != 0 {
		t.Fatalf("batched alert sent before the window closed: %d", len(sender.events))
	}
	queued.snapshot()[0]()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if len(sender.events) != 1 {
		t.Fatalf("flushed events = %d, want 1", len(sender.events))
	}
	got := sender.events[0]
	names := map[string]int{}
	for _, client := range got.Clients {
		names[client.Name]++
	}
	if len(got.Clients) != 2 || names["alpha"] != 1 || names["beta"] != 1 {
		t.Fatalf("merged clients = %#v", got.Clients)
	}
	if !got.Time.Equal(second.Time) {
		t.Fatalf("merged time = %s", got.Time)
	}
}

func TestDigestSequentialCallsShareOneWindow(t *testing.T) {
	sender := &recordingSender{}
	queued, _ := newDigestQueue()
	restore := installDigestTest(t, sender, true, queued)
	defer restore()

	first := models.EventMessage{
		Kind: KindLoad, Event: "Alert", Emoji: "⚠️", Message: "CPU",
		Clients: []models.Client{{UUID: "a", Name: "alpha"}},
	}
	second := models.EventMessage{
		Kind: KindLoad, Event: "Alert", Emoji: "⚠️", Message: "CPU",
		Clients: []models.Client{{UUID: "b", Name: "beta"}},
	}
	if err := SendEvent(first); err != nil {
		t.Fatal(err)
	}
	if err := SendEvent(second); err != nil {
		t.Fatal(err)
	}
	if len(sender.events) != 0 {
		t.Fatalf("sent before the window closed: %d", len(sender.events))
	}
	if queued.len() != 1 {
		t.Fatalf("windows = %d, want 1", queued.len())
	}
	queued.snapshot()[0]()
	if len(sender.events) != 1 || len(sender.events[0].Clients) != 2 {
		t.Fatalf("merged = %#v", sender.events)
	}
}

func TestDigestKeepsDifferentMessagesApart(t *testing.T) {
	sender := &recordingSender{}
	queued, wait := newDigestQueue()
	restore := installDigestTest(t, sender, true, queued)
	defer restore()

	done := make(chan error, 2)
	go func() {
		done <- SendEvent(models.EventMessage{Kind: KindLoad, Event: "Alert", Message: "cpu", Clients: []models.Client{{UUID: "a", Name: "alpha"}}})
	}()
	go func() {
		done <- SendEvent(models.EventMessage{Kind: KindLoad, Event: "Alert", Message: "memory", Clients: []models.Client{{UUID: "b", Name: "beta"}}})
	}()
	waitForDigest(t, wait, 2)
	jobs := queued.snapshot()
	jobs[0]()
	jobs[1]()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if len(sender.events) != 2 {
		t.Fatalf("flushed events = %d, want 2", len(sender.events))
	}
}

func TestDigestFlushReachesEveryChannel(t *testing.T) {
	structured := &recordingSender{}
	plain := &textSender{}
	queued, _ := newDigestQueue()
	restore := installDigestTest(t, structured, true, queued)
	defer restore()
	mu.Lock()
	providers["plain"] = plain
	mu.Unlock()
	channelsForKind = func(kind string) ([]string, error) {
		if kind == KindLoad {
			return []string{"ok", "plain"}, nil
		}
		return nil, nil
	}

	if err := SendEvent(models.EventMessage{
		Kind: KindLoad, Event: "Alert", Emoji: "⚠️", Message: "CPU",
		Clients: []models.Client{{UUID: "a", Name: "alpha"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := SendEvent(models.EventMessage{
		Kind: KindLoad, Event: "Alert", Emoji: "⚠️", Message: "CPU",
		Clients: []models.Client{{UUID: "b", Name: "beta"}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(structured.events) != 0 || plain.texts != 0 {
		t.Fatalf("sent before the window closed: events=%d texts=%d", len(structured.events), plain.texts)
	}
	if queued.len() != 1 {
		t.Fatalf("windows = %d, want 1", queued.len())
	}
	queued.snapshot()[0]()
	if len(structured.events) != 1 || len(structured.events[0].Clients) != 2 {
		t.Fatalf("structured = %#v", structured.events)
	}
	if plain.texts != 1 || plain.last != "Clients: alpha, beta" {
		t.Fatalf("text channel texts=%d body=%q", plain.texts, plain.last)
	}
}

func TestDigestLeavesLoginAndTrafficReportsImmediate(t *testing.T) {
	sender := &recordingSender{}
	queued, _ := newDigestQueue()
	restore := installDigestTest(t, sender, true, queued)
	defer restore()

	for _, kind := range []string{KindLogin, KindTrafficReportDaily, KindTrafficReportWeekly, KindTrafficReportMonthly} {
		before := len(sender.events)
		if err := SendEvent(models.EventMessage{Kind: kind, Event: kind, Message: "report"}); err != nil {
			t.Fatal(err)
		}
		if queued.len() != 0 || len(sender.events) != before+1 {
			t.Fatalf("%s queued %d events %d", kind, queued.len(), len(sender.events))
		}
	}
	if err := SendEvent(models.EventMessage{Kind: KindOnline, Event: "Online", Clients: []models.Client{{UUID: "a", Name: "alpha"}}}); err != nil {
		t.Fatal(err)
	}
	if queued.len() != 1 || len(sender.events) != 4 {
		t.Fatalf("online recovery queued %d events %d", queued.len(), len(sender.events))
	}
}

func TestDigestDisabledSendsImmediately(t *testing.T) {
	sender := &recordingSender{}
	queued, _ := newDigestQueue()
	restore := installDigestTest(t, sender, false, queued)
	defer restore()

	if err := SendEvent(models.EventMessage{Kind: KindOffline, Event: "Offline", Clients: []models.Client{{UUID: "a", Name: "alpha"}}}); err != nil {
		t.Fatal(err)
	}
	if queued.len() != 0 || len(sender.events) != 1 {
		t.Fatalf("queued %d events %d", queued.len(), len(sender.events))
	}
}

func installDigestTest(t *testing.T, sender *recordingSender, digest bool, queued *digestQueue) func() {
	t.Helper()
	prevLoad := loadNotificationSettings
	prevAudit := writeSendAudit
	prevRoutes := channelsForKind
	prevSchedule := scheduleDigest
	mu.Lock()
	prevProviders := providers
	providers = map[string]factory.IMessageSender{"ok": sender}
	mu.Unlock()
	digestMu.Lock()
	prevBatches := digestBatches
	digestBatches = map[string]*digestBatch{}
	digestMu.Unlock()

	writeSendAudit = func(string, string, string, string, map[string]string) {}
	loadNotificationSettings = func(map[string]any) (map[string]any, error) {
		return map[string]any{
			config.NotificationEnabledKey:       true,
			config.NotificationTemplateKey:      "Clients: {{client}}",
			config.NotificationDigestEnabledKey: digest,
			config.NotificationDigestSecondsKey: DefaultDigestSeconds,
		}, nil
	}
	channelsForKind = func(string) ([]string, error) { return []string{"ok"}, nil }
	scheduleDigest = func(_ time.Duration, fn func()) func() {
		queued.add(fn)
		return func() {}
	}
	return func() {
		loadNotificationSettings = prevLoad
		writeSendAudit = prevAudit
		channelsForKind = prevRoutes
		scheduleDigest = prevSchedule
		mu.Lock()
		providers = prevProviders
		mu.Unlock()
		digestMu.Lock()
		digestBatches = prevBatches
		digestMu.Unlock()
	}
}

func waitForDigest(t *testing.T, wait <-chan struct{}, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	got := 0
	for got < count && time.Now().Before(deadline) {
		select {
		case <-wait:
			got++
		case <-time.After(time.Until(deadline)):
		}
	}
	if got < count {
		t.Fatalf("scheduled %d windows, want %d", got, count)
	}
}

type digestQueue struct {
	mu    sync.Mutex
	jobs  []func()
	ready chan struct{}
}

func newDigestQueue() (*digestQueue, <-chan struct{}) {
	queue := &digestQueue{ready: make(chan struct{}, 4)}
	return queue, queue.ready
}

func (queue *digestQueue) add(fn func()) {
	queue.mu.Lock()
	queue.jobs = append(queue.jobs, fn)
	queue.mu.Unlock()
	queue.ready <- struct{}{}
}

func (queue *digestQueue) snapshot() []func() {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	copied := make([]func(), len(queue.jobs))
	copy(copied, queue.jobs)
	return copied
}

func (queue *digestQueue) len() int {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return len(queue.jobs)
}
