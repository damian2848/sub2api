package geminiweb

// Late failures only evict the generation that produced them. An old same-PSID
// request cannot invalidate or replace the newly exported browser session.
func (c *Client) InvalidateCredential(cred Credential) {
	key := credentialKey(cred.Cookie, cred.AuthUser, cred.AuthUserSet, cred.CacheGeneration)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, key)
	delete(c.quotaCache, key)
	delete(c.liveCookies, key)
}
