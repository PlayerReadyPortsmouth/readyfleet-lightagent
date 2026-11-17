package transport

// Reconnect drops only the live transport. Run rebuilds TLS from the enrolled
// certificate paths on the next connection; it never changes pause preference.
func (c *Client) Reconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelCurrent != nil {
		c.cancelCurrent()
	}
}
