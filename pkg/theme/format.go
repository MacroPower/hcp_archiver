package theme

import "fmt"

// TimeLayout is the layout every surface uses to print a timestamp: minute
// precision, since archive entries are minutes-to-years apart and seconds
// would only widen the column.
const TimeLayout = "2006-01-02 15:04"

// CountNoun renders "N singular" or "N plural", so a count reads the same on
// every surface that reports one.
func CountNoun(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}

	return fmt.Sprintf("%d %s", n, plural)
}

// HumanBytes renders a byte count in binary (IEC) units to one decimal place,
// so a size reads the same on every surface: the panel's byte and rate
// readouts, the summary block, and the browser's list descriptions.
func HumanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	units := [...]string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

	value, exp := float64(n)/unit, 0 // exp indexes units; 0 is KiB (one divide).
	for value >= unit && exp < len(units)-1 {
		value /= unit
		exp++
	}

	// Rounding to one decimal can lift a value just under the next unit up to a
	// bare "1024.0"; promote it so the label stays compact (1.0 MiB, not
	// 1024.0 KiB). The 0.05 margin is half the printed precision.
	if value >= unit-0.05 && exp < len(units)-1 {
		value /= unit
		exp++
	}

	return fmt.Sprintf("%.1f %s", value, units[exp])
}
