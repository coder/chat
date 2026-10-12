package slackapi

import "context"

// AuthTestResponse is the identity of a token that auth.test returns.
type AuthTestResponse struct {
	// TeamID is the ID of the workspace of the token.
	TeamID string `json:"team_id"`
	// Team is the name of the workspace.
	Team string `json:"team"`
	// UserID is the ID of the user of the token. For a bot token, it is the
	// bot user.
	UserID string `json:"user_id"`
	// BotID is the bot ID of a bot token. It is empty for a user token.
	BotID string `json:"bot_id"`
	// URL is the workspace URL, for example "https://example.slack.com/".
	URL string `json:"url"`
}

// AuthTest calls auth.test and returns the identity of the token of c.
func (c *Client) AuthTest(ctx context.Context) (*AuthTestResponse, error) {
	var resp AuthTestResponse
	if err := c.Call(ctx, "auth.test", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
