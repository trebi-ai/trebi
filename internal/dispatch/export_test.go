package dispatch

// WindowLen is a test hook: stored rate-limit window size for key.
func (p *Policy) WindowLen(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.window[key])
}
