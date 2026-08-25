package worker

import "strconv"

// cfItoa / cfFtoa are click-funnel-local formatters. Prefixed because package
// worker already carries an `itoa` in its test files.
func cfItoa(v int) string { return strconv.Itoa(v) }

func cfFtoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
