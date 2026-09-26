package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
)

func TestMigrateKeepsExistingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	// A database as the first release created it: base schema only, user_version 0.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema + `INSERT INTO users(email, name, role, created) VALUES('mom@x.com', 'Mom Smith', 'admin', 1);
		INSERT INTO devices(name, token_hash, created, last_seen) VALUES('desktop', '` + HashToken("old-token") + `', 1, 5);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	u, err := st.GetUser(ctx, "mom@x.com")
	if err != nil || u.Name != "Mom Smith" || u.Role != "admin" || u.DisplayName != "" {
		t.Fatalf("user after migrate = %+v, %v", u, err)
	}
	// A chosen name survives the profile refresh done at every Google sign-in.
	_ = st.SetDisplayName(ctx, "mom@x.com", "Mum")
	_ = st.UpdateProfile(ctx, "mom@x.com", "Mom Smith", "")
	if u, _ = st.GetUser(ctx, "mom@x.com"); u.DisplayName != "Mum" {
		t.Fatalf("display name lost: %+v", u)
	}
	// A PC paired before installs existed keeps working with its token.
	if d, err := st.DeviceByToken(ctx, "old-token"); err != nil || d.Name != "desktop" || d.InstallID == 0 {
		t.Fatalf("old token after migrate = %+v, %v", d, err)
	}
	if ds, _ := st.ListDevices(ctx); len(ds) != 1 || len(ds[0].Installs) != 1 || ds[0].Installs[0].Label != "desktop" || ds[0].Installs[0].LastSeen != 5 {
		t.Fatalf("devices after migrate = %+v", ds)
	}
	if s, _ := st.GetSettings(ctx); s.DeliverOffline {
		t.Fatal("offline delivery should default to off")
	}
	st.Close()
	// Reopening is a no-op.
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st2.Close()
}

func TestGroupsAndNames(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	pair := func(name string) int64 {
		code, _ := st.CreatePairingCode(ctx, "x")
		p, err := st.PairDevice(ctx, code, name, "")
		if err != nil {
			t.Fatal(err)
		}
		return p.DeviceID
	}
	desk, lap := pair("desktop"), pair("laptop")

	gid, err := st.SaveGroup(ctx, 0, "Upstairs", []int64{desk, lap, 999})
	if err != nil {
		t.Fatal(err)
	}
	gs, _ := st.ListGroups(ctx)
	if len(gs) != 1 || !slices.Equal(gs[0].DeviceIDs, []int64{desk, lap}) {
		t.Fatalf("groups = %+v", gs)
	}
	if _, err := st.SaveGroup(ctx, 0, "upstairs", nil); err != ErrConflict {
		t.Fatalf("duplicate group name: %v", err)
	}
	if _, ds, err := st.GroupByName(ctx, "UPSTAIRS"); err != nil || len(ds) != 2 {
		t.Fatalf("GroupByName = %v, %v", ds, err)
	}

	for _, c := range []struct {
		name     string
		dev, grp int64
		want     bool
	}{
		{"Desktop", 0, 0, true},     // another PC, any case
		{"desktop", desk, 0, false}, // renaming a PC to its own name
		{"upstairs", 0, 0, true},    // a group
		{"upstairs", 0, gid, false}, // the group itself
		{"kitchen", 0, 0, false},
	} {
		if got, _ := st.NameTaken(ctx, c.name, c.dev, c.grp); got != c.want {
			t.Errorf("NameTaken(%q, %d, %d) = %v", c.name, c.dev, c.grp, got)
		}
	}

	if err := st.RenameDevice(ctx, lap, "desktop"); err != ErrConflict {
		t.Fatalf("rename onto taken name: %v", err)
	}
	if err := st.RenameDevice(ctx, lap, "study"); err != nil {
		t.Fatal(err)
	}
	if d, err := st.DeviceByName(ctx, "study"); err != nil || d.ID != lap {
		t.Fatalf("renamed device = %+v, %v", d, err)
	}
	// Deleting a PC drops it from its groups.
	_ = st.DeleteDevice(ctx, desk)
	if gs, _ = st.ListGroups(ctx); !slices.Equal(gs[0].DeviceIDs, []int64{lap}) {
		t.Fatalf("after delete: %+v", gs)
	}
}

func TestInstallsMergeSplit(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	pair := func(name, replaces string) *Pairing {
		code, _ := st.CreatePairingCode(ctx, "x")
		p, err := st.PairDevice(ctx, code, name, replaces)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	linux, win := pair("desktop", ""), pair("desktop-win", "")
	gid, _ := st.SaveGroup(ctx, 0, "Upstairs", []int64{win.DeviceID})
	_ = st.UpsertUser(ctx, "mom@x.com", "admin")
	if _, err := st.CreateAPIKey(ctx, "Alexa", "mom@x.com", nil, &win.DeviceID); err != nil {
		t.Fatal(err)
	}
	aid, _ := st.CreateMissedAlert(ctx, win.DeviceID, nil)

	if err := st.MergeDevices(ctx, win.DeviceID, win.DeviceID); err == nil {
		t.Fatal("merging a PC into itself should fail")
	}
	if err := st.MergeDevices(ctx, win.DeviceID, linux.DeviceID); err != nil {
		t.Fatal(err)
	}
	ds, _ := st.ListDevices(ctx)
	if len(ds) != 1 || ds[0].Name != "desktop" || len(ds[0].Installs) != 2 || ds[0].Installs[1].Label != "desktop-win" {
		t.Fatalf("after merge: %+v", ds)
	}
	if d, err := st.DeviceByToken(ctx, win.Token); err != nil || d.ID != linux.DeviceID {
		t.Fatalf("windows token after merge = %+v, %v", d, err)
	}
	if gs, _ := st.ListGroups(ctx); !slices.Equal(gs[0].DeviceIDs, []int64{linux.DeviceID}) || gs[0].ID != gid {
		t.Fatalf("groups after merge: %+v", gs)
	}
	if ks, _ := st.ListAPIKeys(ctx); *ks[0].PinnedDevice != linux.DeviceID {
		t.Fatalf("key pin after merge: %+v", ks[0])
	}
	if a, _ := st.GetAlert(ctx, aid); a.DeviceID != linux.DeviceID {
		t.Fatalf("history after merge: %+v", a)
	}

	// Pairing under the same name adds an install; re-pairing with the old token replaces it.
	third := pair("desktop", "") // the pair handler maps any case to the stored name
	if third.DeviceID != linux.DeviceID {
		t.Fatalf("same-name pairing made a new PC: %+v", third)
	}
	again := pair("desktop", third.Token)
	if again.ReplacedInstall != third.InstallID || again.ReplacedDevice != linux.DeviceID {
		t.Fatalf("re-pair should report the replaced install: %+v", again)
	}
	if _, err := st.DeviceByToken(ctx, third.Token); err != ErrNotFound {
		t.Fatalf("replaced token still works: %v", err)
	}
	if err := st.RemoveInstall(ctx, linux.DeviceID, again.InstallID); err != nil {
		t.Fatal(err)
	}

	// Splitting the Windows install off again makes it its own PC.
	if _, err := st.SplitInstall(ctx, linux.DeviceID, win.InstallID, "desktop"); err != ErrConflict {
		t.Fatalf("split onto a taken name: %v", err)
	}
	if _, err := st.SplitInstall(ctx, linux.DeviceID+99, win.InstallID, "x"); err != ErrNotFound {
		t.Fatalf("split from the wrong PC: %v", err)
	}
	wid, err := st.SplitInstall(ctx, linux.DeviceID, win.InstallID, "gaming")
	if err != nil {
		t.Fatal(err)
	}
	if d, err := st.DeviceByToken(ctx, win.Token); err != nil || d.ID != wid || d.Name != "gaming" {
		t.Fatalf("windows token after split = %+v, %v", d, err)
	}
	if err := st.RemoveInstall(ctx, wid, win.InstallID); err != ErrLastInstall {
		t.Fatalf("removing the last install: %v", err)
	}
	if _, err := st.SplitInstall(ctx, linux.DeviceID, linux.InstallID, "y"); err != ErrLastInstall {
		t.Fatalf("splitting the last install: %v", err)
	}
}

func TestParseAgent(t *testing.T) {
	for ua, want := range map[string][2]string{
		"attention-getter/1.1.0 (windows)": {"1.1.0", "windows"},
		"attention-getter/1.2.0":           {"1.2.0", ""},
		"":                                 {"", ""},
		"curl/8":                           {"", ""},
	} {
		if v, os := parseAgent(ua); v != want[0] || os != want[1] {
			t.Errorf("parseAgent(%q) = %q, %q", ua, v, os)
		}
	}
}
