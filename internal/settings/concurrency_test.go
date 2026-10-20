package settings

import (
	"testing"

	"github.com/mailquay/mailquay/internal/platform"
)

func TestAutoConcurrency(t *testing.T) {
	cases := []struct {
		res           platform.Resources
		sync, fetch   int
		search, batch int
	}{
		{platform.Resources{CPUs: 1, MemoryBytes: 4 << 30}, 1, 2, 1, 100},
		{platform.Resources{CPUs: 2, MemoryBytes: 8 << 30}, 2, 4, 2, 200},
		{platform.Resources{CPUs: 8, MemoryBytes: 16 << 30}, 6, 16, 8, 500},
		{platform.Resources{CPUs: 32, MemoryBytes: 512 << 20}, 6, 2, 8, 500},
	}
	for _, tc := range cases {
		got := AutoConcurrency(tc.res, Defaults())
		if got.SyncConcurrency != tc.sync || got.FetchConcurrency != tc.fetch ||
			got.SearchConcurrency != tc.search || got.IndexBatchSize != tc.batch {
			t.Errorf("AutoConcurrency(%+v) = %+v", tc.res, got)
		}
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
}
