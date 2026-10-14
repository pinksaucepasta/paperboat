package configsync

import (
	"context"
	"reflect"
	"testing"
)

func TestSplitRepositoryAppliesPullThenPreparesAgainstPushHead(t *testing.T) {
	events := []string{}
	pull := &observingRepository{fakeRepository: &fakeRepository{events: &events, fetches: []RemoteSnapshot{{Revision: "pull-head"}}, prepared: PreparedPublication{ExpectedRemoteRevision: "pull-head", CommitID: "pull-head"}}}
	push := &fakeRepository{events: &events, fetches: []RemoteSnapshot{{Revision: "push-head"}}, prepared: PreparedPublication{ExpectedRemoteRevision: "push-head", CommitID: "push-next", HasChanges: true}}
	repository := &SplitRepository{Pull: pull, Push: push}
	remote, err := repository.Fetch(context.Background())
	if err != nil || remote.Revision != "push-head" {
		t.Fatalf("remote=%+v err=%v", remote, err)
	}
	prepared, err := repository.Reconcile(context.Background(), remote)
	if err != nil || prepared.CommitID != "push-next" {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
	if !reflect.DeepEqual(events, []string{"fetch", "fetch", "reconcile:pull-head", "reconcile:push-head"}) {
		t.Fatalf("events=%v", events)
	}
	if !reflect.DeepEqual(pull.committed, []string{"pull-head:pull-head"}) {
		t.Fatalf("pull baseline=%v", pull.committed)
	}
}
