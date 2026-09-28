package slackapi

// FileOriginsForTest returns the normalized file origins of c.
func (c *Client) FileOriginsForTest() []string {
	return c.fileOrigins
}
