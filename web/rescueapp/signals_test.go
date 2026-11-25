package rescueapp

import "testing"

func TestRescueSignalOnlyLivesWhileWatched(t *testing.T) {
	const key = "session:signal-test"
	signalKey(key)
	if signals.byKey[key] != nil {
		t.Fatal("notification retained an unwatched session")
	}
	first, releaseFirst := SubscribeSignal(key)
	second, releaseSecond := SubscribeSignal(key)
	defer releaseFirst()
	defer releaseSecond()
	old := first()
	if second() != old {
		t.Fatal("subscribers did not share the same notification")
	}
	signalKey(key)
	select {
	case <-old:
	default:
		t.Fatal("the current notification was not delivered")
	}
	if first() == old {
		t.Fatal("notification was not renewed")
	}
	releaseFirst()
	releaseFirst()
	if signals.byKey[key] == nil {
		t.Fatal("first release removed a second subscriber")
	}
	releaseSecond()
	if signals.byKey[key] != nil {
		t.Fatal("last release did not remove the key")
	}
}
