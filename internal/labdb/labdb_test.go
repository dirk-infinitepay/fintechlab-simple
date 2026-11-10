package labdb

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestEnvironmentsAreKeptWithTheirTokenConfigKitAndRegistry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lab.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := Environment{ID: "sbx", Name: "sbx", Token: "lenv_1", CreatedAt: time.Now(),
		Config: Config{SettlementIdentifier: "Infinite_Payment", WebhookHosts: []string{"alb.example"}},
		Kit:    Kit{BankingCircleUsername: "sbx-bc", B4BKeyID: "sbx-1"}}
	if err := db.Create(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(ctx, e); !errors.Is(err, ErrExists) {
		t.Fatalf("second create: %v", err)
	}
	if err := db.SaveRegistry(ctx, "sbx", []byte(`{"seq": 3}`)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Reopened, as after a lab API restart.
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.ByToken(ctx, "lenv_1")
	if err != nil || got.ID != "sbx" || got.Config.SettlementIdentifier != "Infinite_Payment" || got.Kit.B4BKeyID != "sbx-1" {
		t.Fatalf("ByToken = %+v, %v", got, err)
	}
	if doc, _ := db.Registry(ctx, "sbx"); string(doc) != `{"seq": 3}` {
		t.Fatalf("registry = %s", doc)
	}
	got.Token = "lenv_2"
	if err := db.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ByToken(ctx, "lenv_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the rotated-out token still works: %v", err)
	}
	if err := db.Delete(ctx, "sbx"); err != nil {
		t.Fatal(err)
	}
	if doc, _ := db.Registry(ctx, "sbx"); doc != nil {
		t.Fatalf("the registry outlived its environment: %s", doc)
	}
	if list, _ := db.List(ctx); len(list) != 0 {
		t.Fatalf("List after delete = %+v", list)
	}
}
