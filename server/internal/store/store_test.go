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
	if _, err := db.Exec(schema + `INSERT INTO users(email, name, role, created) VALUES('mom@x.com', 'Mom Smith', 'admin', 1);`); err != nil {
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
		id, _, err := st.PairDevice(ctx, code, name)
		if err != nil {
			t.Fatal(err)
		}
		return id
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
