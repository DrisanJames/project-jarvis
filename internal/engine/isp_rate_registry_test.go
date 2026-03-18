package engine

import (
	"sync"
	"testing"
	"time"
)

func TestSetRate_UpdatesLimiter(t *testing.T) {
	r := NewISPRateRegistry()
	r.SetRate(ISPGmail, 500)

	got := r.GetRate(ISPGmail)
	if got != 500 {
		t.Errorf("GetRate(gmail) = %v, want 500", got)
	}

	r.SetRate(ISPGmail, 250)
	got = r.GetRate(ISPGmail)
	if got != 250 {
		t.Errorf("GetRate(gmail) after update = %v, want 250", got)
	}
}

func TestAllowN_RespectsRate(t *testing.T) {
	r := NewISPRateRegistry()
	// 1 msg/hour = practically 0 tokens per second
	r.SetRate(ISPGmail, 1)

	// First call should allow 1 (the burst minimum)
	got := r.AllowN("gmail", 5)
	if got < 1 {
		t.Errorf("first AllowN should allow at least 1 (burst), got %d", got)
	}

	// Immediately after, tokens are exhausted
	got = r.AllowN("gmail", 5)
	if got != 0 {
		t.Errorf("second AllowN should return 0 (exhausted), got %d", got)
	}
}

func TestAllowN_PartialBatch(t *testing.T) {
	r := NewISPRateRegistry()
	// 3600 msgs/hour = 1 msg/second, burst = max(1, 3600/360) = 10
	r.SetRate(ISPGmail, 3600)

	// Consume all initial burst tokens
	r.AllowN("gmail", 20)

	// Wait a few seconds for partial tokens to regenerate
	time.Sleep(3 * time.Second)

	got := r.AllowN("gmail", 20)
	// Should get ~3 (3 seconds * 1/sec), not 20
	if got > 5 || got < 1 {
		t.Errorf("AllowN(20) after 3s at 1/s should return ~3, got %d", got)
	}
}

func TestAllowN_DefaultsToPermissive(t *testing.T) {
	r := NewISPRateRegistry()
	// "other" is not in the registry
	got := r.AllowN("other", 50)
	if got != 50 {
		t.Errorf("AllowN for unknown ISP should return requested (50), got %d", got)
	}

	got = r.AllowN("verizon", 100)
	if got != 100 {
		t.Errorf("AllowN for unknown ISP should return requested (100), got %d", got)
	}
}

func TestGetAllRates_ReturnsSnapshot(t *testing.T) {
	r := NewISPRateRegistry()
	r.SetRate(ISPGmail, 500)
	r.SetRate(ISPYahoo, 300)
	r.SetRate(ISPComcast, 100)

	all := r.GetAllRates()
	if len(all) != 3 {
		t.Fatalf("expected 3 rates, got %d", len(all))
	}
	if all[ISPGmail] != 500 {
		t.Errorf("gmail = %v, want 500", all[ISPGmail])
	}
	if all[ISPYahoo] != 300 {
		t.Errorf("yahoo = %v, want 300", all[ISPYahoo])
	}
	if all[ISPComcast] != 100 {
		t.Errorf("comcast = %v, want 100", all[ISPComcast])
	}

	// Mutating returned map must not affect the registry
	all[ISPGmail] = 9999
	if r.GetRate(ISPGmail) != 500 {
		t.Error("mutating GetAllRates result should not change the registry")
	}
}

func TestGetAllRates_EmptyRegistry(t *testing.T) {
	r := NewISPRateRegistry()
	all := r.GetAllRates()
	if len(all) != 0 {
		t.Errorf("expected 0 rates for empty registry, got %d", len(all))
	}
}

func TestConcurrentAccess(t *testing.T) {
	r := NewISPRateRegistry()
	r.SetRate(ISPGmail, 500)
	r.SetRate(ISPYahoo, 300)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			r.SetRate(ISPGmail, float64(300+i))
		}(i)
		go func() {
			defer wg.Done()
			r.AllowN("gmail", 5)
			r.AllowN("yahoo", 3)
			r.GetRate(ISPGmail)
		}()
	}
	wg.Wait()
}

func TestRestoreFromDB_NilDB(t *testing.T) {
	r := NewISPRateRegistry()
	r.SetRate(ISPGmail, 500)
	restored := r.RestoreFromDB(map[ISP]float64{ISPGmail: 500})
	if restored != 0 {
		t.Errorf("RestoreFromDB with nil DB should return 0, got %d", restored)
	}
	if r.GetRate(ISPGmail) != 500 {
		t.Errorf("rate should remain unchanged at 500, got %.0f", r.GetRate(ISPGmail))
	}
}

func TestSetDB_NilDB_NoWritePanic(t *testing.T) {
	r := NewISPRateRegistry()
	r.SetRate(ISPGmail, 500)
	r.SetRate(ISPGmail, 250)
	if r.GetRate(ISPGmail) != 250 {
		t.Errorf("rate should be 250 without DB, got %.0f", r.GetRate(ISPGmail))
	}
}
