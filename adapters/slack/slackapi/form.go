package slackapi

import (
	"context"
	"net/url"
	"strconv"
)

// callForm sends values form encoded to the Web API method with the bearer
// token, retries throttling like Call, checks the ok field, and decodes the
// response into dest when dest is not nil.
func (c *Client) callForm(ctx context.Context, method string, values url.Values, dest any) error {
	return c.call(ctx, method, "application/x-www-form-urlencoded", []byte(values.Encode()), dest)
}

// setFormString sets key to value when value is not empty.
func setFormString(values url.Values, key, value string) {
	if value != "" {
		values.Set(key, value)
	}
}

// setFormBool sets key to "true" when value is true.
func setFormBool(values url.Values, key string, value bool) {
	if value {
		values.Set(key, "true")
	}
}

// setFormInt sets key to value when value is not zero.
func setFormInt(values url.Values, key string, value int) {
	if value != 0 {
		values.Set(key, strconv.Itoa(value))
	}
}
