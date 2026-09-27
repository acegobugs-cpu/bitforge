package store_test

import (
	"errors"
	"testing"
	"time"

	"bitforge/lab-runner/internal/store"
)

var all = []store.Status{store.Starting, store.Running, store.Stopped, store.Expired, store.Failed}

func TestStatusTransitionTable(t *testing.T) {
	legal := map[store.Status][]store.Status{
		store.Starting: {store.Running, store.Stopped, store.Failed},
		store.Running:  {store.Stopped, store.Expired, store.Failed},
		// terminal states: nothing
	}
	for _, from := range all {
		for _, to := range all {
			want := false
			for _, l := range legal[from] {
				if l == to {
					want = true
				}
			}
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("%s → %s = %v, want %v", from, to, got, want)
			}
		}
	}
	for _, s := range []store.Status{store.Stopped, store.Expired, store.Failed} {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	if store.Starting.IsTerminal() || store.Running.IsTerminal() {
		t.Error("STARTING / RUNNING must not be terminal")
	}
}

func TestStoreTransitionEnforcesMachineAndOwnsTimestamps(t *testing.T) {
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	st := store.NewStore(func() time.Time { return now })
	launched := now.Add(-time.Minute)
	st.Put(store.Instance{ID: "i", Status: store.Starting, LaunchedAt: launched})

	// legal: mutate applies, but cannot override Status / ID / LaunchedAt
	got, err := st.Transition("i", store.Running, func(x *store.Instance) {
		x.Endpoint = "localhost:32768"
		x.Status = store.Failed // must be ignored
		x.ID = "hijack"         // must be ignored
		x.LaunchedAt = now      // must be ignored
	})
	if err != nil {
		t.Fatalf("legal transition failed: %v", err)
	}
	if got.Status != store.Running || got.ID != "i" || !got.LaunchedAt.Equal(launched) || got.Endpoint != "localhost:32768" {
		t.Errorf("unexpected result after transition: %+v", got)
	}
	if !got.EndedAt.IsZero() {
		t.Error("EndedAt must stay zero on a non-terminal transition")
	}

	// illegal: nothing changes, typed error
	_, err = st.Transition("i", store.Starting, func(x *store.Instance) { x.Endpoint = "mutated" })
	if !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
	after, _ := st.Get("i")
	if after.Status != store.Running || after.Endpoint != "localhost:32768" {
		t.Errorf("illegal transition changed state: %+v", after)
	}

	// terminal: EndedAt stamped from the store clock
	now = now.Add(time.Hour)
	got, err = st.Transition("i", store.Expired, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.EndedAt.Equal(now) {
		t.Errorf("EndedAt = %v, want %v", got.EndedAt, now)
	}
	if _, err := st.Transition("i", store.Running, nil); !errors.Is(err, store.ErrInvalidTransition) {
		t.Error("terminal state must not transition")
	}

	// unknown id
	if _, err := st.Transition("nope", store.Running, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestStoreHandsOutCopies(t *testing.T) {
	st := store.NewStore(nil)
	st.Put(store.Instance{ID: "i", Status: store.Running, Labels: map[string]string{"k": "v"}})
	got, _ := st.Get("i")
	got.Labels["k"] = "changed"
	got.Status = store.Failed
	again, _ := st.Get("i")
	if again.Labels["k"] != "v" || again.Status != store.Running {
		t.Error("Get must return a copy, not internal state")
	}
}

func TestStoreListDeleteLen(t *testing.T) {
	t0 := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	st := store.NewStore(nil)
	st.Put(store.Instance{ID: "b", LaunchedAt: t0.Add(time.Second), Labels: map[string]string{store.LabelOwner: "learn"}})
	st.Put(store.Instance{ID: "a", LaunchedAt: t0, Labels: map[string]string{store.LabelOwner: "learn"}})
	st.Put(store.Instance{ID: "c", LaunchedAt: t0, Labels: map[string]string{store.LabelOwner: "challenge"}})

	got := st.List(store.LabelOwner, "learn")
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("List(owner=learn) = %v", ids(got))
	}
	if n := len(st.List("", "")); n != 3 {
		t.Errorf("List(all) = %d, want 3", n)
	}
	if got := st.List(store.LabelOwner, "nobody"); len(got) != 0 {
		t.Errorf("List(no match) = %v, want empty", ids(got))
	}

	st.Delete("a")
	st.Delete("a") // idempotent
	if _, ok := st.Get("a"); ok || st.Len() != 2 {
		t.Error("Delete did not remove the instance")
	}
}

func TestStoreExpiredUsesClockAndOnlyRunning(t *testing.T) {
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	st := store.NewStore(func() time.Time { return now })

	st.Put(store.Instance{ID: "running-expired", Status: store.Running, ExpiresAt: now.Add(-10 * time.Minute)})
	st.Put(store.Instance{ID: "running-exact", Status: store.Running, ExpiresAt: now}) // boundary: expired
	st.Put(store.Instance{ID: "running-active", Status: store.Running, ExpiresAt: now.Add(10 * time.Minute)})
	st.Put(store.Instance{ID: "starting-expired", Status: store.Starting, ExpiresAt: now.Add(-10 * time.Minute)})
	st.Put(store.Instance{ID: "stopped-expired", Status: store.Stopped, ExpiresAt: now.Add(-10 * time.Minute)})
	st.Put(store.Instance{ID: "failed-expired", Status: store.Failed, ExpiresAt: now.Add(-10 * time.Minute)})

	got := ids(st.Expired())
	if len(got) != 2 || !got["running-expired"] || !got["running-exact"] {
		t.Errorf("Expired() = %v", got)
	}

	// advance the clock: the active one falls due
	now = now.Add(11 * time.Minute)
	if got := ids(st.Expired()); len(got) != 3 || !got["running-active"] {
		t.Errorf("after clock advance Expired() = %v", got)
	}
}

func TestLabelsRoundTrip(t *testing.T) {
	exp := time.Date(2026, time.May, 1, 15, 30, 0, 0, time.UTC)
	original := store.Instance{
		ID:        "c-12345",
		Owner:     "learn",
		SessionID: "sess-99",
		UserID:    "u-1",
		Image:     "ghcr.io/bitforge/labs/hello-flag:1",
		Endpoint:  "localhost:32768",
		ExpiresAt: exp,
		Labels:    map[string]string{"custom.label": "user-value"},
	}

	labels := store.ToLabels(original)
	back, err := store.FromLabels(original.ID, labels, true)
	if err != nil {
		t.Fatalf("FromLabels: %v", err)
	}
	if back.ID != original.ID || back.Owner != "learn" || back.SessionID != "sess-99" || back.UserID != "u-1" ||
		back.Image != original.Image || back.Endpoint != original.Endpoint || !back.ExpiresAt.Equal(exp) {
		t.Errorf("round trip lost data: %+v", back)
	}
	if back.Status != store.Running {
		t.Errorf("running container should rebuild as RUNNING, got %s", back.Status)
	}
	if back.Labels["custom.label"] != "user-value" {
		t.Error("client label dropped")
	}
	if stopped, _ := store.FromLabels("c", labels, false); stopped.Status != store.Stopped {
		t.Errorf("non-running container should rebuild as STOPPED, got %s", stopped.Status)
	}
}

func TestToLabelsReservedKeysWin(t *testing.T) {
	inst := store.Instance{
		Owner: "learn", SessionID: "real", ExpiresAt: time.Now(),
		Labels: map[string]string{store.LabelSessionID: "spoofed", store.LabelOwner: "challenge"},
	}
	labels := store.ToLabels(inst)
	if labels[store.LabelSessionID] != "real" || labels[store.LabelOwner] != "learn" {
		t.Error("client-supplied labels must not override the reserved keys")
	}
}

func TestFromLabelsIsStrict(t *testing.T) {
	good := map[string]string{
		store.LabelOwner:     "learn",
		store.LabelSessionID: "s",
		store.LabelExpiresAt: "2026-05-01T15:30:00Z",
	}
	cases := []struct {
		name string
		drop string
		want error
	}{
		{"no owner", store.LabelOwner, store.ErrMissingOwner},
		{"no session", store.LabelSessionID, store.ErrMissingSessionID},
		{"no expiry", store.LabelExpiresAt, store.ErrBadExpiresAt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			labels := map[string]string{}
			for k, v := range good {
				if k != c.drop {
					labels[k] = v
				}
			}
			if _, err := store.FromLabels("c", labels, true); !errors.Is(err, c.want) {
				t.Errorf("want %v, got %v", c.want, err)
			}
		})
	}
	bad := map[string]string{store.LabelOwner: "learn", store.LabelSessionID: "s", store.LabelExpiresAt: "yesterday"}
	if _, err := store.FromLabels("c", bad, true); !errors.Is(err, store.ErrBadExpiresAt) {
		t.Errorf("unparsable expiry: want ErrBadExpiresAt, got %v", err)
	}
}

func ids(list []store.Instance) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, i := range list {
		out[i.ID] = true
	}
	return out
}
