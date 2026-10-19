package geminiweb

import "testing"

func TestRecoveryGenerationIsolatesSamePSIDAndLateInvalidation(t *testing.T) {
	client := NewClient()
	old := Credential{Cookie: "__Secure-1PSID=same; __Secure-1PSIDTS=old"}
	fresh := Credential{Cookie: "__Secure-1PSID=same; __Secure-1PSIDTS=new", CacheGeneration: "recovered"}
	client.updateSessionCookie(old, old.Cookie)
	if got := client.CurrentCookie(fresh); got != "" {
		t.Fatal("replacement reused rejected cookie")
	}
	client.updateSessionCookie(fresh, fresh.Cookie)
	client.updateSessionCookie(old, "__Secure-1PSID=same; __Secure-1PSIDTS=late")
	client.InvalidateCredential(old)
	if got := client.CurrentCookie(fresh); got != fresh.Cookie {
		t.Fatal("old response modified replacement cache")
	}
	if got := client.CurrentCookie(old); got != "" {
		t.Fatal("old cache survived invalidation")
	}
}
