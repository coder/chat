package slackapi

import (
	"cmp"
	"context"
	"net/url"
)

// User is a Slack user as users.info returns it. The JSON tags are the Slack
// field names.
type User struct {
	// ID is the user ID.
	ID string `json:"id,omitempty"`
	// TeamID is the ID of the workspace of the user.
	TeamID string `json:"team_id,omitempty"`
	// Name is the legacy username.
	Name string `json:"name,omitempty"`
	// RealName is the full name of the user.
	RealName string `json:"real_name,omitempty"`
	// Deleted is true when the user is deactivated.
	Deleted bool `json:"deleted,omitzero"`
	// IsBot is true when the user is a bot user.
	IsBot bool `json:"is_bot,omitzero"`
	// IsStranger is true when the user is in an external workspace that shares
	// a channel with this workspace.
	IsStranger bool `json:"is_stranger,omitzero"`
	// TZ is the IANA time zone of the user, for example "America/New_York".
	TZ string `json:"tz,omitempty"`
	// Locale is the locale of the user, for example "en-US". Slack sends it
	// only when the request sets include_locale.
	Locale string `json:"locale,omitempty"`
	// Profile is the profile of the user.
	Profile UserProfile `json:"profile,omitzero"`
}

// UserProfile is the profile of a User.
type UserProfile struct {
	// DisplayName is the display name that the user chose. It can be empty.
	DisplayName string `json:"display_name,omitempty"`
	// RealName is the full name of the user.
	RealName string `json:"real_name,omitempty"`
	// Email is the email address of the user. Slack sends it only when the
	// token has the users:read.email scope.
	Email string `json:"email,omitempty"`
}

// DisplayName returns the first name of u that is not empty: the profile
// display name, the real name, the profile real name, or the username.
func (u *User) DisplayName() string {
	return cmp.Or(u.Profile.DisplayName, u.RealName, u.Profile.RealName, u.Name)
}

// UserInfoRequest is the request of users.info.
type UserInfoRequest struct {
	// User is the user ID.
	User string `json:"user"`
	// IncludeLocale asks Slack to set User.Locale.
	IncludeLocale bool `json:"include_locale,omitzero"`
}

// UserInfo calls users.info and returns the user. It sends the request form
// encoded. Slack returns the error code "user_not_found" for an unknown user.
func (c *Client) UserInfo(ctx context.Context, req UserInfoRequest) (*User, error) {
	values := url.Values{}
	setFormString(values, "user", req.User)
	setFormBool(values, "include_locale", req.IncludeLocale)
	var resp struct {
		User User `json:"user"`
	}
	if err := c.Call(ctx, "users.info", values, &resp); err != nil {
		return nil, err
	}
	return &resp.User, nil
}
