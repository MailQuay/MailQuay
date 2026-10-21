package sync

import (
	"context"
	"testing"
	"time"

	"github.com/mailquay/mailquay/internal/imapclient"
	"github.com/mailquay/mailquay/internal/netpace"
)

func TestDialIMAPSkipsPacingForInteractive(t *testing.T) {
	svc := &Service{pacer: netpace.New(time.Hour)}
	addr := "127.0.0.1:1"
	if err := svc.pacer.Wait(context.Background(), addr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := svc.dialIMAP(netpace.WithInteractive(ctx), imapclient.Config{
		Address: addr, Security: "none", Timeout: time.Second,
	}); err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
	if ctx.Err() != nil {
		t.Fatal("interactive dial waited for the background pacing slot")
	}
}

func TestBackoffDelay(t *testing.T) {
	interval := 5 * time.Minute
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 0, want: interval},
		{failures: 1, want: 2 * interval},
		{failures: 2, want: 4 * interval},
		{failures: 3, want: 8 * interval},
		{failures: 7, want: 8 * interval},
	}
	for _, tc := range cases {
		if got := backoffDelay(interval, tc.failures); got != tc.want {
			t.Fatalf("backoffDelay(%v, %d) = %v, want %v", interval, tc.failures, got, tc.want)
		}
	}
}
