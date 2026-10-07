package commerce

import "testing"

func TestChannelsReturnsSnapshot(t *testing.T) {
	s := New(nil, "sqlite", nil)
	s.SetChannels(map[string]Channel{"epay": {}})

	got := s.Channels()
	delete(got, "epay")
	got["tokenpay"] = Channel{}

	live := s.Channels()
	if _, ok := live["epay"]; !ok {
		t.Fatal("mutating a Channels result removed the live channel")
	}
	if _, ok := live["tokenpay"]; ok {
		t.Fatal("mutating a Channels result added a live channel")
	}
}
