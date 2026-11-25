package store

import (
	"context"
	"testing"
	"time"
)

// preparePinStore returns a store with one upstream to pin a model on.
func preparePinStore(t *testing.T) (*SQLiteStore, int64) {
	t.Helper()
	store := prepareUpstreamStore(t)
	row := createUpstream(t, store, map[string]any{
		"name":   "Example",
		"url":    "https://api.example.com",
		"keys":   []any{"key-one"},
		"models": []any{"gpt-4.1", "claude-3"},
	})
	siteID, ok := row["id"].(int64)
	if !ok {
		t.Fatalf("created upstream has no id: %v", row)
	}
	return store, siteID
}

func createPin(t *testing.T, store *SQLiteStore, values map[string]any) map[string]any {
	t.Helper()
	row, err := store.CreateResource(context.Background(), "pins", values)
	if err != nil {
		t.Fatalf("CreateResource(pins) error = %v", err)
	}
	return row
}

// A pin is stored per upstream and model, decorated with the upstream's name, and
// attached to every line that upstream serves.
func TestModelPinIsStoredAndAttachedToItsUpstream(t *testing.T) {
	store, siteID := preparePinStore(t)
	ctx := context.Background()
	row := createPin(t, store, map[string]any{
		"site_id":   siteID,
		"model":     "gpt-4.1",
		"mode":      "strict",
		"providers": []any{"alibaba", "baseten"},
		"sort":      "cost",
	})

	if row["site_name"] != "Example" {
		t.Fatalf("site_name = %v, want the upstream's name", row["site_name"])
	}
	if row["mode"] != "strict" {
		t.Fatalf("mode = %v, want strict", row["mode"])
	}

	configuration, err := store.LoadConfiguration(ctx)
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}
	if len(configuration.Channels) != 2 {
		t.Fatalf("channel count = %d, want one per model", len(configuration.Channels))
	}
	for _, channel := range configuration.Channels {
		if len(channel.Pins) != 1 {
			t.Fatalf("channel %s pins = %#v, want one", channel.ID, channel.Pins)
		}
		pin := channel.Pins[0]
		if pin.Model != "gpt-4.1" || pin.Mode != "strict" || len(pin.Providers) != 2 || pin.Sort != "cost" {
			t.Fatalf("pin = %#v, want the stored values", pin)
		}
	}
}

// One pin per upstream and model is what makes "the pin for this model" a
// question with one answer, so a second row is refused.
func TestModelPinIsUniquePerUpstreamAndModel(t *testing.T) {
	store, siteID := preparePinStore(t)
	createPin(t, store, map[string]any{
		"site_id": siteID, "model": "gpt-4.1", "mode": "strict", "providers": []any{"a"},
	})
	if _, err := store.CreateResource(context.Background(), "pins", map[string]any{
		"site_id": siteID, "model": "gpt-4.1", "mode": "preferred", "providers": []any{"b"},
	}); err == nil {
		t.Fatal("a second pin for one upstream and model was accepted")
	}
}

// A pin that names nothing would be stored and never do anything, and a pin on an
// upstream that does not exist would never be read.
func TestModelPinRejectsWhatCouldNeverApply(t *testing.T) {
	store, siteID := preparePinStore(t)
	ctx := context.Background()

	if _, err := store.CreateResource(ctx, "pins", map[string]any{
		"site_id": siteID, "model": "gpt-4.1", "mode": "strict",
	}); err == nil {
		t.Fatal("a pin with no providers and no sort was accepted")
	}

	if _, err := store.CreateResource(ctx, "pins", map[string]any{
		"site_id": 999, "model": "gpt-4.1", "mode": "strict", "providers": []any{"a"},
	}); err == nil {
		t.Fatal("a pin on an upstream that does not exist was accepted")
	}

	if _, err := store.CreateResource(ctx, "pins", map[string]any{
		"site_id": siteID, "model": "gpt-4.1", "mode": "bogus", "providers": []any{"a"},
	}); err == nil {
		t.Fatal("a pin with an unknown mode was accepted")
	}

	if _, err := store.CreateResource(ctx, "pins", map[string]any{
		"site_id": siteID, "model": "gpt-4.1", "mode": "strict",
		"providers": []any{"a", "b"}, "exclude": []any{"a", "b"},
	}); err == nil {
		t.Fatal("a pin that excludes every provider was accepted")
	}
}

// An operator unpins a model by removing the row, so the delete has to find it.
func TestModelPinCanBeDeleted(t *testing.T) {
	store, siteID := preparePinStore(t)
	createPin(t, store, map[string]any{
		"site_id": siteID, "model": "gpt-4.1", "mode": "strict", "providers": []any{"a"},
	})
	removed, err := store.DeleteModelPin(context.Background(), siteID, "gpt-4.1")
	if err != nil {
		t.Fatalf("DeleteModelPin() error = %v", err)
	}
	if !removed {
		t.Fatal("DeleteModelPin() reported nothing removed")
	}
	removed, err = store.DeleteModelPin(context.Background(), siteID, "gpt-4.1")
	if err != nil {
		t.Fatalf("DeleteModelPin() second call error = %v", err)
	}
	if removed {
		t.Fatal("DeleteModelPin() removed a pin twice")
	}
}

// A probe records what it found without overwriting an order the operator set.
func TestSavePinProbeKeepsAConfiguredOrder(t *testing.T) {
	store, siteID := preparePinStore(t)
	createPin(t, store, map[string]any{
		"site_id": siteID, "model": "gpt-4.1", "mode": "strict", "providers": []any{"kept"},
	})
	ctx := context.Background()
	if err := store.SavePinProbe(ctx, siteID, "gpt-4.1", "direct", []string{"one", "two"}, "two", time.Now()); err != nil {
		t.Fatalf("SavePinProbe() error = %v", err)
	}
	configuration, err := store.LoadConfiguration(ctx)
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}
	pin := configuration.Channels[0].Pins[0]
	if pin.Pipeline != "direct" || pin.LastProvider != "two" || pin.ProbedAt.IsZero() {
		t.Fatalf("pin after probe = %#v", pin)
	}
	if len(pin.Providers) != 1 || pin.Providers[0] != "kept" {
		t.Fatalf("providers = %#v, want the configured order kept", pin.Providers)
	}
}

// The pin listing survives a database that predates the table: routing must not
// depend on an optional table existing.
func TestLoadConfigurationToleratatesAMissingPinTable(t *testing.T) {
	store, _ := preparePinStore(t)
	if _, err := store.db.Exec(`DROP TABLE gateway_model_pins`); err != nil {
		t.Fatalf("drop the pin table: %v", err)
	}
	if _, err := store.LoadConfiguration(context.Background()); err != nil {
		t.Fatalf("LoadConfiguration() with no pin table error = %v", err)
	}
}

// The delete of an upstream takes its pins with it: a pin can never outlive the
// upstream it describes.
func TestDeletingAnUpstreamRemovesItsPins(t *testing.T) {
	store, siteID := preparePinStore(t)
	createPin(t, store, map[string]any{
		"site_id": siteID, "model": "gpt-4.1", "mode": "strict", "providers": []any{"a"},
	})
	removed, err := store.DeleteResource(context.Background(), "upstreams", siteID)
	if err != nil {
		t.Fatalf("DeleteResource(upstreams) error = %v", err)
	}
	if removed["pins"] != 1 {
		t.Fatalf("removed = %#v, want the pin counted", removed)
	}
	rows, err := store.ListResource(context.Background(), "pins")
	if err != nil {
		t.Fatalf("ListResource(pins) error = %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("pins after deleting the upstream = %#v", rows)
	}
}
