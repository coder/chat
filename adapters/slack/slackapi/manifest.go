package slackapi

// Manifest is a Slack app manifest in its JSON form. It holds the fields that
// a bot app needs. Pass it to Slack to create or update an app from a
// manifest.
type Manifest struct {
	// Metadata is the optional _metadata section.
	Metadata *ManifestMetadata `json:"_metadata,omitempty"`
	// DisplayInformation is the required display_information section.
	DisplayInformation ManifestDisplayInformation `json:"display_information"`
	// Features is the optional features section.
	Features *ManifestFeatures `json:"features,omitempty"`
	// OAuthConfig is the optional oauth_config section.
	OAuthConfig *ManifestOAuthConfig `json:"oauth_config,omitempty"`
	// Settings is the optional settings section.
	Settings *ManifestSettings `json:"settings,omitempty"`
}

// ManifestMetadata is the _metadata section of a Manifest. It holds the
// manifest schema version.
type ManifestMetadata struct {
	// MajorVersion is the major_version. It is omitted when zero.
	MajorVersion int `json:"major_version,omitzero"`
	// MinorVersion is the minor_version. It is omitted when zero.
	MinorVersion int `json:"minor_version,omitzero"`
}

// ManifestDisplayInformation is the display_information section of a
// Manifest.
type ManifestDisplayInformation struct {
	// Name is the app name.
	Name string `json:"name"`
	// Description is the optional short description.
	Description string `json:"description,omitempty"`
	// LongDescription is the optional long description.
	LongDescription string `json:"long_description,omitempty"`
	// BackgroundColor is the optional background color as a hex code, such
	// as "#4A154B".
	BackgroundColor string `json:"background_color,omitempty"`
}

// ManifestFeatures is the features section of a Manifest.
type ManifestFeatures struct {
	// BotUser is the optional bot_user section.
	BotUser *ManifestBotUser `json:"bot_user,omitempty"`
}

// ManifestBotUser is the features.bot_user section of a Manifest.
type ManifestBotUser struct {
	// DisplayName is the display name of the bot user.
	DisplayName string `json:"display_name"`
	// AlwaysOnline makes the bot user always show as online.
	AlwaysOnline bool `json:"always_online"`
}

// ManifestOAuthConfig is the oauth_config section of a Manifest.
type ManifestOAuthConfig struct {
	// Scopes is the optional scopes section.
	Scopes *ManifestScopes `json:"scopes,omitempty"`
}

// ManifestScopes is the oauth_config.scopes section of a Manifest.
type ManifestScopes struct {
	// Bot is the list of bot token scopes, such as "chat:write".
	Bot []string `json:"bot,omitempty"`
}

// ManifestSettings is the settings section of a Manifest. The three flags
// are always encoded, so false is explicit.
type ManifestSettings struct {
	// EventSubscriptions is the optional event_subscriptions section.
	EventSubscriptions *ManifestEventSubscriptions `json:"event_subscriptions,omitempty"`
	// Interactivity is the optional interactivity section.
	Interactivity *ManifestInteractivity `json:"interactivity,omitempty"`
	// OrgDeployEnabled enables org-wide deployment on Enterprise Grid.
	OrgDeployEnabled bool `json:"org_deploy_enabled"`
	// SocketModeEnabled makes Slack send events over Socket Mode instead of
	// HTTP.
	SocketModeEnabled bool `json:"socket_mode_enabled"`
	// TokenRotationEnabled enables token rotation.
	TokenRotationEnabled bool `json:"token_rotation_enabled"`
}

// ManifestEventSubscriptions is the settings.event_subscriptions section of
// a Manifest.
type ManifestEventSubscriptions struct {
	// RequestURL is the optional URL that receives events over HTTP.
	RequestURL string `json:"request_url,omitempty"`
	// BotEvents is the list of events that the bot subscribes to, such as
	// "app_mention".
	BotEvents []string `json:"bot_events,omitempty"`
}

// ManifestInteractivity is the settings.interactivity section of a
// Manifest.
type ManifestInteractivity struct {
	// IsEnabled enables interactive components.
	IsEnabled bool `json:"is_enabled"`
	// RequestURL is the optional URL that receives interaction payloads.
	RequestURL string `json:"request_url,omitempty"`
}
