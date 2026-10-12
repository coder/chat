package slackapi

import (
	"net/url"
	"strconv"
)

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
