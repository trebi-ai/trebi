package config

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/platform"
)

// UserConfig is what setup learns about the person.
type UserConfig struct {
	Name      string   `yaml:"name,omitempty" json:"name,omitempty"`
	TimeZone  string   `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	Interests []string `yaml:"interests,omitempty" json:"interests,omitempty"`
}

// UserInterests are the valid user.interests ids. The UI owns the labels.
var UserInterests = []string{"email", "reports", "code", "research", "sales", "watch"} //nolint:gochecknoglobals // fixed list of ids

const userNameMax = 80

// TimeZoneOrLocal is user.timezone, or the zone of this machine, or "".
func (u UserConfig) TimeZoneOrLocal() string {
	return cmp.Or(u.TimeZone, platform.LocalTimeZone())
}

// setUserKey handles user.* keys; handled=false when path is not one.
func (c *Config) setUserKey(path, value string) (handled bool, err error) {
	switch path {
	case "user.name":
		name := clearOrValue(value)
		if n := len([]rune(name)); n > userNameMax {
			return true, fmt.Errorf("user.name: %d characters, at most %d", n, userNameMax)
		}
		c.User.Name = name
	case "user.timezone":
		tz := clearOrValue(value)
		if tz != "" {
			if _, err := time.LoadLocation(tz); err != nil {
				return true, fmt.Errorf("user.timezone: %q is not an IANA time zone, for example Europe/Berlin", tz)
			}
		}
		c.User.TimeZone = tz
	case "user.interests":
		list, err := parseStringSlice(value, true)
		if err != nil {
			return true, fmt.Errorf("user.interests: %w", err)
		}
		for _, v := range list {
			if !slices.Contains(UserInterests, v) {
				return true, fmt.Errorf("user.interests: %q is not one of %s", v, strings.Join(UserInterests, ", "))
			}
		}
		c.User.Interests = list
	default:
		return false, nil
	}
	return true, nil
}
