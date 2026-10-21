package settings

import (
	"testing"

	"github.com/mailquay/mailquay/internal/platform"
)

func TestEffectiveConcurrency(t *testing.T) {
	cases := []struct {
		res           platform.Resources
		sync, fetch   int
		search, batch int
	}{
		{platform.Resources{CPUs: 1, MemoryBytes: 4 << 30}, 2, 2, 1, 50},
		{platform.Resources{CPUs: 2, MemoryBytes: 8 << 30}, 2, 2, 1, 100},
		{platform.Resources{CPUs: 4, MemoryBytes: 8 << 30}, 2, 2, 2, 200},
		{platform.Resources{CPUs: 8, MemoryBytes: 16 << 30}, 2, 2, 4, 250},
		{platform.Resources{CPUs: 0, MemoryBytes: 0}, 2, 2, 1, 50},
	}
	for _, tc := range cases {
		got := EffectiveConcurrency(Defaults(), tc.res)
		if got.SyncConcurrency != tc.sync || got.FetchConcurrency != tc.fetch ||
			got.SearchConcurrency != tc.search || got.IndexBatchSize != tc.batch {
			t.Errorf("EffectiveConcurrency(%+v) = %+v", tc.res, got)
		}
	}
	v := Defaults()
	v.SyncConcurrency = 5
	v.FetchConcurrency = 7
	got := EffectiveConcurrency(v, platform.Resources{CPUs: 4})
	if got.SyncConcurrency != 5 || got.FetchConcurrency != 7 {
		t.Fatalf("operator values were overridden: %+v", got)
	}
}

func TestEffectiveConcurrencyHonorsOperatorValues(t *testing.T) {
	v := Defaults()
	v.AutoLocalConcurrency = false
	v.SearchConcurrency = 3
	v.IndexBatchSize = 120
	got := EffectiveConcurrency(v, platform.Resources{CPUs: 8})
	if got.SearchConcurrency != 3 || got.IndexBatchSize != 120 {
		t.Fatalf("manual local pools were overridden: %+v", got)
	}
	v.AutoLocalConcurrency = true
	got = EffectiveConcurrency(v, platform.Resources{CPUs: 8})
	if got.SearchConcurrency != 4 || got.IndexBatchSize != 250 {
		t.Fatalf("automatic local pools did not follow resources: %+v", got)
	}
}

func TestValidate(t *testing.T) {
	v := Defaults()
	if err := validate(v); err != nil {
		t.Fatal(err)
	}
	v.SendRetryLimit = 11
	if err := validate(v); err == nil {
		t.Fatal("expected retry limit rejection")
	}
	v = Defaults()
	v.DefaultDownloadScope = "everything"
	if err := validate(v); err == nil {
		t.Fatal("expected scope rejection")
	}
	v = Defaults()
	v.SearchConcurrency = 9
	if err := validate(v); err == nil {
		t.Fatal("expected search concurrency rejection")
	}
	v = Defaults()
	v.IndexBatchSize = 5
	if err := validate(v); err == nil {
		t.Fatal("expected index batch rejection")
	}
}
