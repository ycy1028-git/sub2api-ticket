package tlsfingerprint

import "testing"

func TestProfileAdvertisesHTTP2(t *testing.T) {
	if (&Profile{}).AdvertisesHTTP2() {
		t.Fatal("empty ALPN must not advertise h2")
	}
	if (&Profile{ALPNProtocols: []string{"http/1.1"}}).AdvertisesHTTP2() {
		t.Fatal("http/1.1 only must not advertise h2")
	}
	if !(&Profile{ALPNProtocols: []string{"h2", "http/1.1"}}).AdvertisesHTTP2() {
		t.Fatal("rustls Codex ALPN must advertise h2")
	}
	var nilProfile *Profile
	if nilProfile.AdvertisesHTTP2() {
		t.Fatal("nil profile must not advertise h2")
	}
}
