package schedule

import "time"

// NextDue is a test hook for the unexported next-due calculator.
func NextDue(spec string, from time.Time, tz string) time.Time {
	return nextDue(spec, from, tz)
}
