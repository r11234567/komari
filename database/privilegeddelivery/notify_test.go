package privilegeddelivery

import (
	"testing"
	"time"
)

func TestWorkspaceCommittedMutationWakesOnlyMatchingWatchers(t *testing.T) {
	db, id := fixtureV2(t)
	changed, cancel := Subscribe(id)
	defer cancel()
	other, cancelOther := Subscribe("other")
	defer cancelOther()
	saved, err := SaveWithDB(db, id, Settings{RequiredPrivilegeMode: 2}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("commit did not wake watcher")
	}
	snapshot, err := GetStateWithDB(db, id)
	if err != nil || snapshot.Desired == nil || snapshot.Desired.Revision != saved.Revision {
		t.Fatalf("watcher woke before committed state: %v %+v", err, snapshot)
	}
	select {
	case <-other:
		t.Fatal("different machine woke")
	default:
	}
	next, release := Subscribe(id)
	release()
	Notify(id)
	select {
	case <-next:
		t.Fatal("cancelled watch retained")
	default:
	}
}
